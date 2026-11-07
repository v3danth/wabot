package wa

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// Profile is what WhatsApp tells us about a user.
type Profile struct {
	JID        types.JID
	About      string
	Devices    int
	PictureURL string // empty when the picture is hidden or not set
}

// LookupProfile gathers About, devices and picture URL in one place so
// callers never call the user APIs directly.
func LookupProfile(ctx context.Context, client *whatsmeow.Client, who types.JID) (Profile, error) {
	infos, err := client.GetUserInfo(ctx, []types.JID{who})
	if err != nil {
		return Profile{}, fmt.Errorf("user info %s: %w", who, err)
	}
	info := infos[who]
	p := Profile{JID: who, About: info.Status, Devices: len(info.Devices)}
	// A missing picture is normal (privacy settings), so it is not an error.
	if pic, err := client.GetProfilePictureInfo(ctx, who, &whatsmeow.GetProfilePictureParams{}); err == nil && pic != nil {
		p.PictureURL = pic.URL
	}
	return p, nil
}
