package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

var (
	friend = types.NewJID("911111111111", types.DefaultUserServer)
	me     = types.NewJID("919886473508", types.DefaultUserServer)
)

func incoming(id types.MessageID, chat, sender types.JID, text string) *events.Message {
	return &events.Message{
		Info:    types.MessageInfo{ID: id, MessageSource: types.MessageSource{Chat: chat, Sender: sender}},
		Message: textMsg(text),
	}
}

func resetRecent() {
	recentMu.Lock()
	defer recentMu.Unlock()
	recentIDs = nil
	recentMap = map[types.MessageID]*events.Message{}
}

func TestParseCommand(t *testing.T) {
	tests := []struct {
		in, name, args string
		ok             bool
	}{
		{"!echo hello world", "echo", "hello world", true},
		{"!HELP", "help", "", true},
		{"hello", "", "", false},
	}
	for _, tc := range tests {
		name, args, ok := parseCommand(tc.in)
		if name != tc.name || args != tc.args || ok != tc.ok {
			t.Errorf("parseCommand(%q) = %q, %q, %v; want %q, %q, %v", tc.in, name, args, ok, tc.name, tc.args, tc.ok)
		}
	}
}

func TestSplitArgs(t *testing.T) {
	got := splitArgs(" Lunch? | pizza || sushi ", "|")
	if want := []string{"Lunch?", "pizza", "sushi"}; !slices.Equal(got, want) {
		t.Errorf("splitArgs = %q, want %q", got, want)
	}
}

func TestPhoneToJID(t *testing.T) {
	if got := phoneToJID("+91 98864-73508"); got != me {
		t.Errorf("phoneToJID = %s, want %s", got, me)
	}
}

func TestToJID(t *testing.T) {
	got, err := toJID("278833789944039@lid")
	if err != nil || got.Server != types.HiddenUserServer || got.User != "278833789944039" {
		t.Errorf("toJID(lid) = %v, %v", got, err)
	}
}

func TestTextOf(t *testing.T) {
	img := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("cap")}}
	if got := textOf(img); got != "cap" {
		t.Errorf("textOf(image) = %q, want caption", got)
	}
	if got := kindOf(img); got != "imageMessage" {
		t.Errorf("kindOf(image) = %q", got)
	}
}

func TestExtFor(t *testing.T) {
	if got := extFor("application/x-unknown-thing"); got != ".bin" {
		t.Errorf("extFor(unknown) = %q, want .bin", got)
	}
}

func TestResolveEmoji(t *testing.T) {
	if got := resolveEmoji(""); got != defaultReaction {
		t.Errorf("resolveEmoji(\"\") = %q", got)
	}
	if got := resolveEmoji("random"); !slices.Contains(randomEmojis, got) {
		t.Errorf("resolveEmoji(random) = %q, not in list", got)
	}
	if got := resolveEmoji("x"); got != "x" {
		t.Errorf("resolveEmoji(x) = %q", got)
	}
}

func TestByPosition(t *testing.T) {
	resetRecent()
	remember(incoming("OLD", friend, friend, "first"))
	remember(incoming("NEW", friend, friend, "second"))
	for in, want := range map[string]types.MessageID{"last": "NEW", "#1": "NEW", "#2": "OLD"} {
		evt, ok := byPosition(in)
		if !ok || evt.Info.ID != want {
			t.Errorf("byPosition(%q) = %v, %v; want %s", in, evt, ok, want)
		}
	}
	if _, ok := byPosition("#3"); ok {
		t.Error("byPosition(#3) found a message past the end")
	}
}

func TestPickTarget(t *testing.T) {
	resetRecent()
	remember(incoming("3AABC", friend, friend, "hi"))
	term := Env{Msg: incoming("", me, me, "!react"), Terminal: true}

	term.Args = "3aabc x"
	tg, rest, err := pickTarget(term)
	if err != nil || tg.ID != "3AABC" || tg.Chat != friend || rest != "x" {
		t.Errorf("by id: %+v %q %v", tg, rest, err)
	}

	term.Args = "x"
	if _, _, err := pickTarget(term); err == nil {
		t.Error("terminal without id: want error")
	}

	quoted := Env{Args: "x", Msg: &events.Message{
		Info: types.MessageInfo{ID: "CMD", MessageSource: types.MessageSource{Chat: friend, Sender: friend}},
		Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String("!react x"),
			ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("OLD"), Participant: proto.String(me.String())},
		}},
	}}
	if tg, _, _ := pickTarget(quoted); tg.ID != "OLD" || tg.Sender != me {
		t.Errorf("quoted: %+v", tg)
	}

	plain := Env{Args: "x", Msg: incoming("CMD", friend, friend, "!react x")}
	if tg, _, _ := pickTarget(plain); tg.ID != "CMD" {
		t.Errorf("own message: %+v", tg)
	}
}

func TestProfileText(t *testing.T) {
	got := profileText(profile{JID: friend, About: "busy", Devices: 2})
	want := "*911111111111@s.whatsapp.net*\nAbout: busy\nDevices: 2\nProfile pic: hidden or none"
	if got != want {
		t.Errorf("profileText =\n%s\nwant\n%s", got, want)
	}
}

func TestChatLogLine(t *testing.T) {
	en := chatEntry{
		At:   time.Date(2026, 11, 7, 14, 3, 21, 0, time.UTC),
		ID:   "3AABC",
		Who:  "me",
		Kind: "conversation",
		Text: "two\nlines",
	}
	want := "2026-11-07 14:03:21 [3AABC] me <conversation>: two\\nlines\n"
	if got := chatLogLine(en); got != want {
		t.Errorf("chatLogLine = %q, want %q", got, want)
	}
}

func TestChatLogPath(t *testing.T) {
	jid := types.NewADJID("919886473508", 0, 3)
	if got := chatLogPath(jid); strings.Contains(filepath.Base(got), ":") {
		t.Errorf("chatLogPath(%s) = %q, contains ':'", jid, got)
	}
}

func TestEntryText(t *testing.T) {
	react := &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key:  &waCommon.MessageKey{ID: proto.String("3AABC")},
		Text: proto.String("x"),
	}}
	if got := entryText(react); got != "reacted x to 3AABC" {
		t.Errorf("entryText(reaction) = %q", got)
	}
	react.ReactionMessage.Text = proto.String("")
	if got := entryText(react); got != "removed reaction from 3AABC" {
		t.Errorf("entryText(unreact) = %q", got)
	}
}
