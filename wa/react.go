package wa

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// DefaultReaction is thumbs up, written as an escape to keep the source free
// of literal emoji.
const DefaultReaction = "\U0001F44D"

// Target is the message a reaction or reply acts on. WhatsApp identifies a
// message by chat + sender + ID, never by ID alone.
type Target struct {
	Chat, Sender types.JID
	ID           types.MessageID
	Message      *waE2E.Message // the original, needed only for quoting
}

// SendReaction reacts to the target; an empty emoji removes the reaction.
func SendReaction(ctx context.Context, client *whatsmeow.Client, t Target, emoji string) error {
	_, err := client.SendMessage(ctx, t.Chat, client.BuildReaction(t.Chat, t.Sender, t.ID, emoji))
	return err
}
