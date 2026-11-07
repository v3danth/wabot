package wa

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// ReactionMessageID is the existing message the flow reacts to.
// Replace this placeholder with the ID of a message sent by this account.
const ReactionMessageID types.MessageID = "3EB0TEST"

// ErrNotOnWhatsApp lets callers such as the HTTP API tell a bad number
// apart from a network failure.
var ErrNotOnWhatsApp = errors.New("not on WhatsApp")

// FlowState is passed from step to step; each step returns a new copy.
type FlowState struct {
	Phone     string    // digits as typed; names the saved photo
	Who       types.JID // filled by checkRegistered
	Profile   Profile   // filled by fetchProfile
	PhotoDir  string    // where savePhoto writes
	PhotoPath string    // filled by savePhoto; empty when there is no picture
	Emoji     string    // reaction to put on the message
	MessageID types.MessageID
	Reaction  Sent         // filled by reactToMessage once WhatsApp acks it
	Timings   []StepTiming // one entry per finished step, filled by RunSteps
}

// StepTiming records how long one step took, so callers can see where the
// time of a request went.
type StepTiming struct {
	Step string
	Took time.Duration
}

// FlowStep is one stage of the flow. Steps are plain functions so the
// order is just a list and new stages can be added without new branching.
type FlowStep struct {
	Name string
	Run  func(context.Context, *whatsmeow.Client, FlowState) (FlowState, error)
}

// ContactFlowSteps is the order the contact flow runs in.
var ContactFlowSteps = []FlowStep{
	{"check registered", checkRegistered},
	{"whois", fetchProfile},
	{"save photo", savePhoto},
	{"react", reactToMessage},
}

// ContactFlow is the function to call from code: phone in, finished state out.
func ContactFlow(ctx context.Context, client *whatsmeow.Client, phone string) (FlowState, error) {
	jid := PhoneToJID(phone)
	start := FlowState{
		Phone:     jid.User,
		Who:       jid,
		PhotoDir:  PhotoDir(),
		Emoji:     DefaultReaction,
		MessageID: ReactionMessageID,
	}
	return RunSteps(ctx, client, start, ContactFlowSteps)
}

// RunSteps runs steps in order and stops at the first failure, naming the
// step so the caller knows how far it got.
func RunSteps(ctx context.Context, client *whatsmeow.Client, st FlowState, steps []FlowStep) (FlowState, error) {
	for i, step := range steps {
		start := time.Now()
		next, err := step.Run(ctx, client, st)
		if err != nil {
			return st, fmt.Errorf("step %d/%d %q: %w", i+1, len(steps), step.Name, err)
		}
		took := time.Since(start)
		slog.Info("flow step done", "step", step.Name, "took", took)
		// Copy before appending so the caller's slice is never shared.
		next.Timings = append(slices.Clone(st.Timings), StepTiming{step.Name, took})
		st = next
	}
	return st, nil
}

// checkRegistered turns the typed number into the JID WhatsApp knows, and
// fails early for numbers that are not on WhatsApp.
func checkRegistered(ctx context.Context, client *whatsmeow.Client, st FlowState) (FlowState, error) {
	res, err := client.IsOnWhatsApp(ctx, []string{"+" + st.Who.User})
	if err != nil {
		return st, err
	}
	if len(res) == 0 || !res[0].IsIn {
		return st, fmt.Errorf("%s: %w", st.Who.User, ErrNotOnWhatsApp)
	}
	st.Who = res[0].JID
	return st, nil
}

func fetchProfile(ctx context.Context, client *whatsmeow.Client, st FlowState) (FlowState, error) {
	p, err := LookupProfile(ctx, client, st.Who)
	if err != nil {
		return st, err
	}
	st.Profile = p
	return st, nil
}

// savePhoto stores the profile picture on disk. A hidden picture is normal,
// so the step then does nothing instead of failing the flow.
func savePhoto(ctx context.Context, _ *whatsmeow.Client, st FlowState) (FlowState, error) {
	if st.Profile.PictureURL == "" {
		return st, nil
	}
	path, err := SaveProfilePhoto(ctx, st.Profile.PictureURL, st.PhotoDir, st.Phone)
	if err != nil {
		return st, err
	}
	st.PhotoPath = path
	return st, nil
}

// reactToMessage reacts to the configured existing message. The sender in the
// key is our own JID, so the target message must have been sent by this account.
func reactToMessage(ctx context.Context, client *whatsmeow.Client, st FlowState) (FlowState, error) {
	t := Target{Chat: st.Who, Sender: client.Store.ID.ToNonAD(), ID: st.MessageID}
	sent, err := SendReaction(ctx, client, t, st.Emoji)
	if err != nil {
		return st, err
	}
	st.Reaction = sent
	return st, nil
}
