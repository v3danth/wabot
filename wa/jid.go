package wa

import (
	"strings"

	"go.mau.fi/whatsmeow/types"
)

// PhoneToJID turns "+91 98765-43210" into 919876543210@s.whatsapp.net.
func PhoneToJID(phone string) types.JID {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, phone)
	return types.NewJID(digits, types.DefaultUserServer)
}

// ToJID accepts a full JID (…@lid, …@g.us, …@s.whatsapp.net) or a plain phone number.
func ToJID(s string) (types.JID, error) {
	if strings.Contains(s, "@") {
		return types.ParseJID(s)
	}
	return PhoneToJID(s), nil
}
