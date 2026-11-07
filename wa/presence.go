package wa

import (
	"context"
	"fmt"
	"sync"
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

// presenceTracker remembers the latest presence of every user. WhatsApp sends
// the current state only on the first subscription and then only changes, so
// a repeat lookup must be answered from what was already received.
type presenceTracker struct {
	mu      sync.Mutex
	last    map[types.JID]Presence
	changed chan struct{} // closed and replaced on every update, to wake waiters
}

// trackers holds one tracker per client so its event handler is added once.
var trackers sync.Map // *whatsmeow.Client -> *presenceTracker

func trackerFor(client *whatsmeow.Client) *presenceTracker {
	if t, ok := trackers.Load(client); ok {
		return t.(*presenceTracker)
	}
	t := &presenceTracker{last: map[types.JID]Presence{}, changed: make(chan struct{})}
	if actual, loaded := trackers.LoadOrStore(client, t); loaded {
		return actual.(*presenceTracker)
	}
	client.AddEventHandler(t.handle)
	return t
}

func (t *presenceTracker) handle(raw any) {
	switch evt := raw.(type) {
	case *events.Presence:
		t.mu.Lock()
		t.last[evt.From.ToNonAD()] = presenceOf(evt)
		close(t.changed)
		t.changed = make(chan struct{})
		t.mu.Unlock()
	case *events.Connected:
		// Subscriptions end with the connection, so cached states would go stale.
		t.mu.Lock()
		clear(t.last)
		t.mu.Unlock()
	}
}

// lookup returns the latest known presence for any of ids, plus a channel
// that is closed on the next update.
func (t *presenceTracker) lookup(ids []types.JID) (Presence, bool, <-chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, id := range ids {
		if p, ok := t.last[id]; ok {
			return p, true, t.changed
		}
	}
	return Presence{}, false, t.changed
}

// FetchPresence returns whether who is online. WhatsApp has no "get presence"
// call: it pushes updates as events after a subscription, so this subscribes
// and waits up to wait for the first one unless one is already known.
func FetchPresence(ctx context.Context, client *whatsmeow.Client, who types.JID, wait time.Duration) (Presence, error) {
	tracker := trackerFor(client)
	// The server only sends presence to clients that are themselves online.
	if err := client.SendPresence(ctx, types.PresenceAvailable); err != nil {
		return Presence{}, fmt.Errorf("set own presence: %w", err)
	}
	if err := client.SubscribePresence(ctx, who); err != nil {
		return Presence{}, fmt.Errorf("subscribe: %w", err)
	}
	// Presence may arrive keyed by phone number or by LID.
	ids := []types.JID{who.ToNonAD()}
	if alt, err := client.Store.GetAltJID(ctx, who); err == nil && !alt.IsEmpty() {
		ids = append(ids, alt.ToNonAD())
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		p, ok, changed := tracker.lookup(ids)
		if ok {
			return p, nil
		}
		select {
		case <-changed:
		case <-timer.C:
			return Presence{Status: PresenceUnknown}, nil
		case <-ctx.Done():
			return Presence{}, ctx.Err()
		}
	}
}

func presenceOf(evt *events.Presence) Presence {
	if !evt.Unavailable {
		return Presence{Status: PresenceOnline}
	}
	return Presence{Status: PresenceOffline, LastSeen: evt.LastSeen}
}
