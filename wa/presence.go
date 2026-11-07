package wa

import (
	"context"
	"fmt"
	"slices"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// PresenceWait bounds how long the flow waits for WhatsApp to push a
// presence update after subscribing. Updates usually arrive within a second.
const PresenceWait = 5 * time.Second

// Presence values as reported to callers.
const (
	PresenceOnline  = "online"
	PresenceOffline = "offline"
	PresenceUnknown = "unknown" // no update arrived: hidden by privacy or slow
)

// Presence is whether a user is online right now.
type Presence struct {
	Status   string
	LastSeen time.Time // zero when they hide last seen or are online
}

// FetchPresence subscribes to a user's presence and returns the first update.
// WhatsApp has no "get presence" call: it only pushes updates as events after
// a subscription, so this waits for one, up to wait.
func FetchPresence(ctx context.Context, client *whatsmeow.Client, who types.JID, wait time.Duration) (Presence, error) {
	// The server only sends presence to clients that are themselves online.
	if err := client.SendPresence(ctx, types.PresenceAvailable); err != nil {
		return Presence{}, fmt.Errorf("set own presence: %w", err)
	}
	ids := []types.JID{who.ToNonAD()}
	if alt, err := client.Store.GetAltJID(ctx, who); err == nil && !alt.IsEmpty() {
		ids = append(ids, alt.ToNonAD())
	}
	updates := make(chan *events.Presence, 1)
	handler := client.AddEventHandler(func(raw any) {
		evt, ok := raw.(*events.Presence)
		if !ok || !slices.Contains(ids, evt.From.ToNonAD()) {
			return
		}
		select {
		case updates <- evt:
		default: // the first update is enough
		}
	})
	defer client.RemoveEventHandler(handler)

	if err := client.SubscribePresence(ctx, who); err != nil {
		return Presence{}, fmt.Errorf("subscribe: %w", err)
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case evt := <-updates:
		return presenceOf(evt), nil
	case <-timer.C:
		return Presence{Status: PresenceUnknown}, nil
	case <-ctx.Done():
		return Presence{}, ctx.Err()
	}
}

func presenceOf(evt *events.Presence) Presence {
	if !evt.Unavailable {
		return Presence{Status: PresenceOnline}
	}
	return Presence{Status: PresenceOffline, LastSeen: evt.LastSeen}
}
