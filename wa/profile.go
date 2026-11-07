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
	Name       string // best known display name; empty when we have never seen one
	NameSource string // where Name came from: contact, push name, business
	About      string // empty when hidden by their privacy settings or not set
	Devices    int
	PictureURL string // empty when the picture is hidden or not set
}

// LookupProfile gathers name, About, devices and picture URL in one place so
// callers never call the user APIs directly.
func LookupProfile(ctx context.Context, client *whatsmeow.Client, who types.JID) (Profile, error) {
	infos, err := client.GetUserInfo(ctx, []types.JID{who})
	if err != nil {
		return Profile{}, fmt.Errorf("user info %s: %w", who, err)
	}
	info := infos[who]
	p := Profile{JID: who, About: info.Status, Devices: len(info.Devices)}
	p.Name, p.NameSource = displayName(ctx, client, who)
	if p.Name == "" && info.VerifiedName != nil {
		p.Name, p.NameSource = info.VerifiedName.Details.GetVerifiedName(), "verified business"
	}
	// A missing picture is normal (privacy settings), so it is not an error.
	if pic, err := client.GetProfilePictureInfo(ctx, who, &whatsmeow.GetProfilePictureParams{}); err == nil && pic != nil {
		p.PictureURL = pic.URL
	}
	return p, nil
}

// displayName reads the local contact store. WhatsApp has no "get name" call:
// a name is only known if they are in your address book, have messaged you
// (push name), or are a business. The store may key them by phone or by LID,
// so both forms are tried.
func displayName(ctx context.Context, client *whatsmeow.Client, who types.JID) (name, source string) {
	jids := []types.JID{who}
	if alt, err := client.Store.GetAltJID(ctx, who); err == nil && !alt.IsEmpty() {
		jids = append(jids, alt)
	}
	for _, jid := range jids {
		c, err := client.Store.Contacts.GetContact(ctx, jid)
		if err != nil || !c.Found {
			continue
		}
		if name, source = contactName(c); name != "" {
			return name, source
		}
	}
	return "", ""
}

// contactName prefers the name you saved over the name they chose.
func contactName(c types.ContactInfo) (name, source string) {
	switch {
	case c.FullName != "":
		return c.FullName, "contact"
	case c.FirstName != "":
		return c.FirstName, "contact"
	case c.PushName != "":
		return c.PushName, "push name"
	case c.BusinessName != "":
		return c.BusinessName, "business"
	}
	return "", ""
}
