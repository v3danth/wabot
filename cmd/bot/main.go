// Command bot is an interactive WhatsApp bot showing most whatsmeow features,
// written as small functions + a command table.
//
// Run from the repo root so it shares session.db with the API:
//
//	go run ./cmd/bot
//
// First run shows a QR code -> WhatsApp > Settings > Linked devices > Link a device.
// The session is saved in session.db, so later runs log in automatically.
//
// Then send "!help" to the linked number (from another phone, or from your
// own phone in the "Message yourself" chat).
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"math/rand/v2"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"wabot/wa"

	_ "github.com/mattn/go-sqlite3" // SQLite driver (stores your login session)
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	dbPath      = "session.db"
	downloadDir = "downloads"
	chatLogDir  = "chats"
	prefix      = "!"
)

// =====================================================================
// 2. Pure helpers (no network, easy to test)
// =====================================================================

// textOf extracts the plain text / caption of a message.
func textOf(m *waE2E.Message) string {
	switch {
	case m.GetConversation() != "":
		return m.GetConversation()
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetText()
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetCaption()
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetCaption()
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage().GetCaption()
	}
	return ""
}

// parseCommand splits "!echo hello world" into ("echo", "hello world").
func parseCommand(text string) (name, args string, ok bool) {
	if !strings.HasPrefix(text, prefix) {
		return "", "", false
	}
	name, args, _ = strings.Cut(strings.TrimPrefix(text, prefix), " ")
	return strings.ToLower(name), strings.TrimSpace(args), true
}

// splitArgs splits "a | b | c" into ["a","b","c"].
func splitArgs(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// media is a downloadable attachment and its mimetype.
type media interface {
	whatsmeow.DownloadableMessage
	GetMimetype() string
}

// mediaOf returns the attachment in a message, if any. Picking the type here
// is what whatsmeow recommends over the deprecated DownloadAny.
func mediaOf(m *waE2E.Message) (media, bool) {
	switch {
	case m.GetImageMessage() != nil:
		return m.GetImageMessage(), true
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage(), true
	case m.GetAudioMessage() != nil:
		return m.GetAudioMessage(), true
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage(), true
	case m.GetStickerMessage() != nil:
		return m.GetStickerMessage(), true
	}
	return nil, false
}

// extFor picks a file extension for a mimetype.
func extFor(mimetype string) string {
	base, _, _ := strings.Cut(mimetype, ";")
	if exts, _ := mime.ExtensionsByType(base); len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

// demoPNG draws a small gradient image in memory (so no file is needed).
func demoPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	for x := range 256 {
		for y := range 256 {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 180, 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// ---- message builders ----

func textMsg(text string) *waE2E.Message {
	return &waE2E.Message{Conversation: proto.String(text)}
}

func imageMsg(up whatsmeow.UploadResponse, mimetype, caption string) *waE2E.Message {
	return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption:       proto.String(caption),
		Mimetype:      proto.String(mimetype),
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		MediaKey:      up.MediaKey,
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    proto.Uint64(up.FileLength),
	}}
}

func documentMsg(up whatsmeow.UploadResponse, mimetype, fileName string) *waE2E.Message {
	return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		Title:         proto.String(fileName),
		FileName:      proto.String(fileName),
		Mimetype:      proto.String(mimetype),
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		MediaKey:      up.MediaKey,
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    proto.Uint64(up.FileLength),
	}}
}

func locationMsg(lat, lng float64, name string) *waE2E.Message {
	return &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
		DegreesLatitude:  proto.Float64(lat),
		DegreesLongitude: proto.Float64(lng),
		Name:             proto.String(name),
	}}
}

// =====================================================================
// 3. Commands — each is a plain function: (ctx, client, msg, args) -> error
// =====================================================================

// Env is everything a command needs.
type Env struct {
	Ctx    context.Context
	Client *whatsmeow.Client
	Msg    *events.Message
	Args   string
	// Terminal is true when the command was typed in the terminal, not sent on WhatsApp.
	// Text replies then print in the terminal; messages/media go to your "Message yourself" chat.
	Terminal bool
}

// Chat is the chat the command came from.
func (e Env) Chat() types.JID { return e.Msg.Info.Chat }

// Send sends a message to the current chat and returns its ID.
func (e Env) Send(m *waE2E.Message) (types.MessageID, error) {
	resp, err := sendMessage(e.Ctx, e.Client, e.Chat(), m)
	return resp.ID, err
}

// Say replies with text: printed in the terminal, or sent to the WhatsApp chat.
func (e Env) Say(format string, a ...any) error {
	if e.Terminal {
		fmt.Println(fmt.Sprintf(format, a...))
		return nil
	}
	_, err := e.Send(textMsg(fmt.Sprintf(format, a...)))
	return err
}

type Command struct {
	Usage string
	Help  string
	Run   func(Env) error
}

// commands is the command table. Add a line here to add a feature.
// It is filled in init() because !help needs to read the table itself.
var commands map[string]Command

func init() {
	commands = map[string]Command{
		// --- basic messaging ---
		"help":      {"!help", "show this list", cmdHelp},
		"ping":      {"!ping", "reply pong", func(e Env) error { return e.Say("pong") }},
		"echo":      {"!echo <text>", "repeat your text", cmdEcho},
		"reply":     {"!reply [id] <text>", "quoted reply (to a message ID, the quoted msg, or yours)", cmdReply},
		"react":     {"!react [id|last|#n] [emoji|random]", "react to a message (ID, newest, #n from recent, quoted, or yours)", cmdReact},
		"unreact":   {"!unreact [id|last|#n]", "remove your reaction", cmdUnreact},
		"reactall":  {"!reactall <emoji> [n]", "react to the last n messages (default 5)", cmdReactAll},
		"autoreact": {"!autoreact <word> <emoji> | list | off <word>", "auto-react when a message contains a word", cmdAutoReact},
		"reactto":   {"!reactto <chat> <id> [emoji] [sender]", "react to ANY message by hand (sender defaults to me)", cmdReactTo},
		"info":      {"!info [id]", "show chat/sender/id/key of a message", cmdInfo},
		"chatlog":   {"!chatlog", "where this chat's saved log is", cmdChatLog},
		"recent":    {"!recent", "list recent message IDs (to use with react/reply)", cmdRecent},
		"edit":      {"!edit", "send a message, then edit it", cmdEdit},
		"del":       {"!del", "send a message, then delete it for everyone", cmdDelete},
		"type":      {"!type", "show 'typing…' then reply", cmdTyping},

		// --- rich messages ---
		"image":    {"!image", "send a generated image", cmdImage},
		"doc":      {"!doc", "send a text file as a document", cmdDoc},
		"location": {"!location", "send a location pin", cmdLocation},
		"poll":     {"!poll Q | opt1 | opt2", "create a poll (votes are logged)", cmdPoll},

		// --- users ---
		"check":  {"!check <phone>", "is this number on WhatsApp?", cmdCheck},
		"whois":  {"!whois [phone|jid]", "About text, devices and profile picture", cmdWhois},
		"pic":    {"!pic [phone|jid]", "only the profile picture", cmdPic},
		"flow":   {"!flow <phone>", "whois, then react to a configured message", cmdFlow},
		"status": {"!status <text>", "set the bot account's About text", cmdStatus},
		"blocks": {"!blocks", "list blocked contacts", cmdBlocklist},

		// --- groups ---
		"groups":    {"!groups", "list groups the bot is in", cmdGroups},
		"groupinfo": {"!groupinfo", "info about this group", cmdGroupInfo},
		"invite":    {"!invite", "get this group's invite link", cmdInvite},
		"mkgroup":   {"!mkgroup <name>", "create a group with you in it", cmdMakeGroup},
		"rename":    {"!rename <name>", "rename this group", cmdRename},

		// --- channels ---
		"channels": {"!channels", "list followed channels (newsletters)", cmdChannels},

		// --- send to anyone ---
		"send": {"!send <phone> <text>", "send a text to any number", cmdSend},
	}
}

func cmdHelp(e Env) error {
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("*Commands*\n")
	for _, n := range names {
		fmt.Fprintf(&b, "`%s` — %s\n", commands[n].Usage, commands[n].Help)
	}
	b.WriteString("\nAlso: send any image/video/audio/doc and I'll save it.")
	return e.Say("%s", b.String())
}

func cmdEcho(e Env) error {
	if e.Args == "" {
		return errors.New("usage: !echo <text>")
	}
	return e.Say("%s", e.Args)
}

func cmdSend(e Env) error {
	phone, text, _ := strings.Cut(e.Args, " ")
	if phone == "" || strings.TrimSpace(text) == "" {
		return errors.New("usage: !send 919876543210 hello there")
	}
	to := wa.PhoneToJID(phone)
	if _, err := sendMessage(e.Ctx, e.Client, to, textMsg(text)); err != nil {
		return err
	}
	return e.Say("Sent to %s", to.User)
}

// ---- recent messages: remembered so you can react/reply by ID ----

const recentMax = 50

var (
	recentMu  sync.Mutex
	recentIDs []types.MessageID                       // oldest first
	recentMap = map[types.MessageID]*events.Message{} // ID -> message
)

// remember stores an incoming message so it can be targeted by ID later.
func remember(evt *events.Message) {
	recentMu.Lock()
	defer recentMu.Unlock()
	recentIDs = append(recentIDs, evt.Info.ID)
	recentMap[evt.Info.ID] = evt
	if len(recentIDs) > recentMax {
		delete(recentMap, recentIDs[0])
		recentIDs = recentIDs[1:]
	}
}

// lookup finds a remembered message by ID (case-insensitive).
func lookup(id string) (*events.Message, bool) {
	recentMu.Lock()
	defer recentMu.Unlock()
	evt, ok := recentMap[types.MessageID(strings.ToUpper(id))]
	return evt, ok
}

// pickTarget decides which message react/reply acts on and returns the leftover args:
//  1. first arg is a known message ID  -> that message (works from terminal too)
//  2. command was sent as a WhatsApp reply to a message -> the quoted message
//  3. otherwise -> the command message itself (WhatsApp only)
func pickTarget(e Env) (wa.Target, string, error) {
	first, rest, _ := strings.Cut(e.Args, " ")
	evt, ok := lookup(first)
	if !ok {
		evt, ok = byPosition(first)
	}
	if ok {
		return wa.Target{Chat: evt.Info.Chat, Sender: evt.Info.Sender, ID: evt.Info.ID, Message: evt.Message}, strings.TrimSpace(rest), nil
	}
	if ci := e.Msg.Message.GetExtendedTextMessage().GetContextInfo(); ci.GetStanzaID() != "" {
		sender, err := types.ParseJID(ci.GetParticipant())
		if err != nil || sender.IsEmpty() {
			sender = e.Chat() // private chat: the other person
		}
		return wa.Target{Chat: e.Chat(), Sender: sender, ID: ci.GetStanzaID(), Message: ci.GetQuotedMessage()}, e.Args, nil
	}
	if e.Terminal {
		return wa.Target{}, "", errors.New("say which message: react <id|last|#n> <emoji> (see: recent)")
	}
	return wa.Target{Chat: e.Chat(), Sender: e.Msg.Info.Sender, ID: e.Msg.Info.ID, Message: e.Msg.Message}, e.Args, nil
}

func cmdRecent(e Env) error {
	recentMu.Lock()
	defer recentMu.Unlock()
	if len(recentIDs) == 0 {
		return e.Say("No messages received yet.")
	}
	var b strings.Builder
	for n := 1; n <= 10 && n <= len(recentIDs); n++ {
		id := recentIDs[len(recentIDs)-n]
		m := recentMap[id]
		fmt.Fprintf(&b, "#%d id=%s\n   chat=%s\n   sender=%s (%s)\n   text=%.40q\n",
			n, id, m.Info.Chat, m.Info.Sender.ToNonAD(), m.Info.PushName, textOf(m.Message))
	}
	return e.Say("%s", b.String())
}

// byPosition resolves "last" (= #1, the newest) or "#n" to a remembered message.
func byPosition(s string) (*events.Message, bool) {
	n := 0
	switch {
	case s == "last":
		n = 1
	case strings.HasPrefix(s, "#"):
		if _, err := fmt.Sscanf(s, "#%d", &n); err != nil {
			return nil, false
		}
	default:
		return nil, false
	}
	recentMu.Lock()
	defer recentMu.Unlock()
	if n < 1 || n > len(recentIDs) {
		return nil, false
	}
	return recentMap[recentIDs[len(recentIDs)-n]], true
}

// Reaction emojis are data sent to WhatsApp, written as escapes to keep the
// source free of literal emoji: thumbs up, heart, fire, joy, wow, sad, pray, party.
var randomEmojis = []string{
	"\U0001F44D", "\u2764\uFE0F", "\U0001F525", "\U0001F602",
	"\U0001F62E", "\U0001F622", "\U0001F64F", "\U0001F389",
}

// resolveEmoji turns "" into the default reaction and "random" into a random one.
func resolveEmoji(s string) string {
	switch s {
	case "":
		return wa.DefaultReaction
	case "random":
		return randomEmojis[rand.IntN(len(randomEmojis))]
	}
	return s
}

// sendReaction is the one place that actually sends a reaction ("" removes it).
func sendReaction(ctx context.Context, client *whatsmeow.Client, t wa.Target, emoji string) error {
	_, err := sendMessage(ctx, client, t.Chat, client.BuildReaction(t.Chat, t.Sender, t.ID, emoji))
	return err
}

func targetOf(evt *events.Message) wa.Target {
	return wa.Target{Chat: evt.Info.Chat, Sender: evt.Info.Sender, ID: evt.Info.ID, Message: evt.Message}
}

func cmdUnreact(e Env) error {
	t, _, err := pickTarget(e)
	if err != nil {
		return err
	}
	if err := sendReaction(e.Ctx, e.Client, t, ""); err != nil {
		return err
	}
	return e.Say("Reaction removed from %s", t.ID)
}

// cmdReactAll reacts to the last n messages. On WhatsApp: only in the current chat.
func cmdReactAll(e Env) error {
	f := strings.Fields(e.Args)
	if len(f) == 0 {
		return errors.New("usage: reactall <emoji|random> [n]")
	}
	n := 5
	if len(f) > 1 {
		if _, err := fmt.Sscanf(f[1], "%d", &n); err != nil || n < 1 || n > 20 {
			return errors.New("n must be 1–20")
		}
	}
	recentMu.Lock()
	var picks []wa.Target
	for i := len(recentIDs) - 1; i >= 0 && len(picks) < n; i-- {
		m := recentMap[recentIDs[i]]
		if e.Terminal || m.Info.Chat == e.Chat() {
			picks = append(picks, targetOf(m))
		}
	}
	recentMu.Unlock()
	for _, t := range picks {
		if err := sendReaction(e.Ctx, e.Client, t, resolveEmoji(f[0])); err != nil {
			return err
		}
		time.Sleep(500 * time.Millisecond) // don't spam WhatsApp
	}
	return e.Say("Reacted to %d message(s)", len(picks))
}

// ---- auto-react rules: word -> emoji ----

var (
	autoMu    sync.Mutex
	autoRules = map[string]string{}
)

func cmdAutoReact(e Env) error {
	f := strings.Fields(e.Args)
	autoMu.Lock()
	defer autoMu.Unlock()
	switch {
	case len(f) == 0 || f[0] == "list":
		if len(autoRules) == 0 {
			return e.Say("No auto-react rules. Add one: autoreact hello <emoji>")
		}
		var b strings.Builder
		for w, em := range autoRules {
			fmt.Fprintf(&b, "%q → %s\n", w, em)
		}
		return e.Say("%s", b.String())
	case f[0] == "off" && len(f) == 2:
		delete(autoRules, strings.ToLower(f[1]))
		return e.Say("Removed rule for %q", f[1])
	case len(f) == 2:
		autoRules[strings.ToLower(f[0])] = f[1]
		return e.Say("Will react %s to messages containing %q", f[1], f[0])
	}
	return errors.New("usage: autoreact <word> <emoji> | list | off <word>")
}

// autoReact applies the first matching rule to an incoming message.
func autoReact(ctx context.Context, client *whatsmeow.Client, evt *events.Message, text string) {
	lower := strings.ToLower(text)
	autoMu.Lock()
	var emoji string
	for w, em := range autoRules {
		if strings.Contains(lower, w) {
			emoji = em
			break
		}
	}
	autoMu.Unlock()
	if emoji == "" {
		return
	}
	if err := sendReaction(ctx, client, targetOf(evt), resolveEmoji(emoji)); err != nil {
		slog.Warn("auto-react failed", "id", evt.Info.ID, "err", err)
		return
	}
	slog.Info("auto-reacted", "id", evt.Info.ID, "chat", evt.Info.Chat)
}

// describeTarget shows the 4 BuildReaction inputs and the key WhatsApp will receive.
func describeTarget(e Env, t wa.Target, emoji string) string {
	key := e.Client.BuildMessageKey(t.Chat, t.Sender, t.ID)
	participant := key.GetParticipant()
	if participant == "" {
		participant = "(none — private chat)"
	}
	return fmt.Sprintf("BuildReaction inputs:\n"+
		"  chat   = %s\n  sender = %s\n  id     = %s\n  emoji  = %q\n"+
		"MessageKey sent to WhatsApp:\n"+
		"  RemoteJID   = %s\n  ID          = %s\n  FromMe      = %v\n  Participant = %s",
		t.Chat, t.Sender.ToNonAD(), t.ID, emoji,
		key.GetRemoteJID(), key.GetID(), key.GetFromMe(), participant)
}

// cmdReactTo reacts using all 4 inputs typed by hand — no remembered message needed.
//
//	reactto 278833789944039@lid 3EB0AC684A7EE08CD8C216 <emoji>               (my own message)
//	reactto 120363…@g.us 3A88EC035EDE6284E0F0 <emoji> 919876543210           (someone else's, in a group)
func cmdReactTo(e Env) error {
	f := strings.Fields(e.Args)
	if len(f) < 2 {
		return errors.New("usage: reactto <chat> <id> [emoji] [sender|me]")
	}
	chat, err := wa.ToJID(f[0])
	if err != nil {
		return fmt.Errorf("bad chat: %w", err)
	}
	t := wa.Target{Chat: chat, ID: strings.ToUpper(f[1]), Sender: *e.Client.Store.ID} // default: my message
	emoji := wa.DefaultReaction
	if len(f) >= 3 {
		emoji = resolveEmoji(f[2])
	}
	if len(f) >= 4 && f[3] != "me" {
		if t.Sender, err = wa.ToJID(f[3]); err != nil {
			return fmt.Errorf("bad sender: %w", err)
		}
	}
	if err := sendReaction(e.Ctx, e.Client, t, emoji); err != nil {
		return err
	}
	return e.Say("%s\nReacted.", describeTarget(e, t, emoji))
}

func cmdInfo(e Env) error {
	t, _, err := pickTarget(e)
	if err != nil {
		return err
	}
	return e.Say("%s", describeTarget(e, t, "<your emoji>"))
}

func cmdReply(e Env) error {
	t, text, err := pickTarget(e)
	if err != nil {
		return err
	}
	if text == "" {
		text = "This is a quoted reply."
	}
	msg := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String(text),
		ContextInfo: &waE2E.ContextInfo{
			StanzaID:      proto.String(t.ID),
			Participant:   proto.String(t.Sender.ToNonAD().String()),
			QuotedMessage: t.Message,
		},
	}}
	if _, err := sendMessage(e.Ctx, e.Client, t.Chat, msg); err != nil {
		return err
	}
	if e.Terminal {
		return e.Say("Replied to %s", t.ID)
	}
	return nil
}

func cmdReact(e Env) error {
	t, emoji, err := pickTarget(e)
	if err != nil {
		return err
	}
	emoji = resolveEmoji(emoji)
	if err := sendReaction(e.Ctx, e.Client, t, emoji); err != nil {
		return err
	}
	if e.Terminal {
		return e.Say("%s\nReacted.", describeTarget(e, t, emoji))
	}
	return nil
}

func cmdEdit(e Env) error {
	id, err := e.Send(textMsg("Original text… (will be edited in 2s)"))
	if err != nil {
		return err
	}
	time.Sleep(2 * time.Second)
	_, err = e.Send(e.Client.BuildEdit(e.Chat(), id, textMsg("Edited text.")))
	return err
}

func cmdDelete(e Env) error {
	id, err := e.Send(textMsg("This message will be deleted in 3s."))
	if err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	// Sender = own JID (empty JID also means "me").
	_, err = e.Send(e.Client.BuildRevoke(e.Chat(), types.EmptyJID, id))
	return err
}

func cmdTyping(e Env) error {
	if err := e.Client.SendChatPresence(e.Ctx, e.Chat(), types.ChatPresenceComposing, types.ChatPresenceMediaText); err != nil {
		return fmt.Errorf("start typing: %w", err)
	}
	time.Sleep(3 * time.Second)
	if err := e.Client.SendChatPresence(e.Ctx, e.Chat(), types.ChatPresencePaused, types.ChatPresenceMediaText); err != nil {
		return fmt.Errorf("stop typing: %w", err)
	}
	return e.Say("Done typing.")
}

func cmdImage(e Env) error {
	data := demoPNG()
	up, err := e.Client.Upload(e.Ctx, data, whatsmeow.MediaImage)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	_, err = e.Send(imageMsg(up, "image/png", "Generated by wabot"))
	return err
}

func cmdDoc(e Env) error {
	data := []byte("Hello from whatsmeow!\nSent at " + time.Now().Format(time.RFC1123) + "\n")
	up, err := e.Client.Upload(e.Ctx, data, whatsmeow.MediaDocument)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	_, err = e.Send(documentMsg(up, "text/plain", "hello.txt"))
	return err
}

func cmdLocation(e Env) error {
	_, err := e.Send(locationMsg(28.6139, 77.2090, "New Delhi"))
	return err
}

func cmdPoll(e Env) error {
	parts := splitArgs(e.Args, "|")
	if len(parts) < 3 {
		return errors.New("usage: !poll Question | option 1 | option 2")
	}
	_, err := e.Send(e.Client.BuildPollCreation(parts[0], parts[1:], 1))
	return err
}

func cmdCheck(e Env) error {
	if e.Args == "" {
		return errors.New("usage: !check <phone with country code>")
	}
	res, err := e.Client.IsOnWhatsApp(e.Ctx, []string{"+" + wa.PhoneToJID(e.Args).User})
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, r := range res {
		fmt.Fprintf(&b, "%s → on WhatsApp: %v (%s)\n", r.Query, r.IsIn, r.JID)
	}
	return e.Say("%s", b.String())
}

// whoisTarget picks whose profile to show: the argument if given, else the sender.
func whoisTarget(e Env) (types.JID, error) {
	if e.Args == "" {
		return e.Msg.Info.Sender.ToNonAD(), nil
	}
	return wa.ToJID(e.Args)
}

// profileText renders a profile for chat or terminal.
func profileText(p wa.Profile) string {
	pic := "hidden or none"
	if p.PictureURL != "" {
		pic = "available"
	}
	name := "unknown"
	if p.Name != "" {
		name = p.Name + " (" + p.NameSource + ")"
	}
	about := p.About
	if about == "" {
		about = "hidden or empty"
	}
	return fmt.Sprintf("*%s*\nName: %s\nAbout: %s\nDevices: %d\nProfile pic: %s", p.JID, name, about, p.Devices, pic)
}

// httpClient has a timeout so a stalled CDN download cannot hang a command.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// fetch downloads a URL into memory.
func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	// Close errors after a full read carry no useful information.
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// saveFile writes into the download folder and returns the absolute path,
// so the printed path works from any directory.
func saveFile(name string, data []byte) (string, error) {
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(downloadDir, name))
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, data, 0o644)
}

// deliverImage hands an image to whoever asked: saved to disk for the
// terminal (it cannot show images), sent as a photo on WhatsApp.
func deliverImage(e Env, data []byte, mimetype, name, caption string) error {
	if e.Terminal {
		path, err := saveFile(name, data)
		if err != nil {
			return fmt.Errorf("save %s: %w", name, err)
		}
		return e.Say("Saved %s\nOpen it with: open %q", path, path)
	}
	up, err := e.Client.Upload(e.Ctx, data, whatsmeow.MediaImage)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	_, err = e.Send(imageMsg(up, mimetype, caption))
	return err
}

// sendProfilePic downloads a profile picture and delivers it.
func sendProfilePic(e Env, p wa.Profile) error {
	if p.PictureURL == "" {
		return errors.New("profile picture is hidden or not set")
	}
	data, err := fetch(e.Ctx, p.PictureURL)
	if err != nil {
		return fmt.Errorf("download profile pic: %w", err)
	}
	return deliverImage(e, data, "image/jpeg", "profile_"+p.JID.User+".jpg", "Profile picture of "+p.JID.User)
}

func cmdWhois(e Env) error {
	who, err := whoisTarget(e)
	if err != nil {
		return err
	}
	p, err := wa.LookupProfile(e.Ctx, e.Client, who)
	if err != nil {
		return err
	}
	if err := e.Say("%s", profileText(p)); err != nil {
		return err
	}
	if p.PictureURL == "" {
		return nil
	}
	return sendProfilePic(e, p)
}

func cmdPic(e Env) error {
	who, err := whoisTarget(e)
	if err != nil {
		return err
	}
	p, err := wa.LookupProfile(e.Ctx, e.Client, who)
	if err != nil {
		return err
	}
	return sendProfilePic(e, p)
}

func cmdFlow(e Env) error {
	phone, extra, _ := strings.Cut(e.Args, " ")
	if phone == "" || strings.TrimSpace(extra) != "" {
		return errors.New("usage: flow <phone>")
	}
	st, err := wa.ContactFlow(e.Ctx, e.Client, phone)
	if err != nil {
		return err
	}
	var steps strings.Builder
	for _, t := range st.Timings {
		fmt.Fprintf(&steps, "\n- %s: %d ms", t.Step, t.Took.Milliseconds())
	}
	return e.Say("%s\n\nReacted %s to message %s (ack %s).\nTimings:%s",
		profileText(st.Profile), st.Emoji, st.MessageID, st.Reaction.ID, steps.String())
}

func cmdStatus(e Env) error {
	if e.Args == "" {
		return errors.New("usage: !status <text>")
	}
	if err := e.Client.SetStatusMessage(e.Ctx, types.SetStatusInput{Text: proto.String(e.Args)}); err != nil {
		return err
	}
	return e.Say("About updated.")
}

func cmdBlocklist(e Env) error {
	bl, err := e.Client.GetBlocklist(e.Ctx, "")
	if err != nil {
		return err
	}
	return e.Say("Blocked contacts: %d", len(bl.Items))
}

func cmdGroups(e Env) error {
	groups, err := e.Client.GetJoinedGroups(e.Ctx)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "*%d groups*\n", len(groups))
	for _, g := range groups {
		fmt.Fprintf(&b, "• %s (%d members)\n", g.Name, len(g.Participants))
	}
	return e.Say("%s", b.String())
}

// requireGroup returns an error if the command wasn't sent in a group.
func requireGroup(e Env) error {
	if !e.Msg.Info.IsGroup {
		return errors.New("this command only works inside a group")
	}
	return nil
}

func cmdGroupInfo(e Env) error {
	if err := requireGroup(e); err != nil {
		return err
	}
	g, err := e.Client.GetGroupInfo(e.Ctx, e.Chat())
	if err != nil {
		return err
	}
	return e.Say("*%s*\nTopic: %s\nMembers: %d\nCreated: %s",
		g.Name, g.Topic, len(g.Participants), g.GroupCreated.Format("2006-01-02"))
}

func cmdInvite(e Env) error {
	if err := requireGroup(e); err != nil {
		return err
	}
	link, err := e.Client.GetGroupInviteLink(e.Ctx, e.Chat(), false)
	if err != nil {
		return fmt.Errorf("bot must be admin: %w", err)
	}
	return e.Say("Invite link: %s", link)
}

func cmdMakeGroup(e Env) error {
	if e.Args == "" {
		return errors.New("usage: !mkgroup <name>")
	}
	g, err := e.Client.CreateGroup(e.Ctx, whatsmeow.ReqCreateGroup{
		Name:         e.Args,
		Participants: []types.JID{e.Msg.Info.Sender.ToNonAD()},
	})
	if err != nil {
		return err
	}
	return e.Say("Created group %q (%s)", g.Name, g.JID)
}

func cmdRename(e Env) error {
	if err := requireGroup(e); err != nil {
		return err
	}
	if e.Args == "" {
		return errors.New("usage: !rename <new name>")
	}
	return e.Client.SetGroupName(e.Ctx, e.Chat(), e.Args)
}

func cmdChannels(e Env) error {
	list, err := e.Client.GetSubscribedNewsletters(e.Ctx)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "*%d channels*\n", len(list))
	for _, n := range list {
		fmt.Fprintf(&b, "• %s\n", n.ThreadMeta.Name.Text)
	}
	return e.Say("%s", b.String())
}

// =====================================================================
// Chat log: every chat is saved to chats/<chat>.txt
// =====================================================================

// chatEntry is one line of a saved chat.
type chatEntry struct {
	At   time.Time
	Chat types.JID
	ID   types.MessageID
	Who  string
	Kind string
	Text string
}

// chatLogPath is the file for one chat; ':' is replaced because device
// suffixes are not valid in every file system.
func chatLogPath(chat types.JID) string {
	return filepath.Join(chatLogDir, strings.ReplaceAll(chat.String(), ":", "_")+".txt")
}

// chatLogLine renders one entry; newlines are escaped so one message is one line.
func chatLogLine(en chatEntry) string {
	text := strings.ReplaceAll(en.Text, "\n", `\n`)
	return fmt.Sprintf("%s [%s] %s <%s>: %s\n", en.At.Format("2006-01-02 15:04:05"), en.ID, en.Who, en.Kind, text)
}

// entryText is the readable content of a message, including reactions,
// which have no text of their own.
func entryText(m *waE2E.Message) string {
	if r := m.GetReactionMessage(); r != nil {
		if r.GetText() == "" {
			return "removed reaction from " + r.GetKey().GetID()
		}
		return "reacted " + r.GetText() + " to " + r.GetKey().GetID()
	}
	return textOf(m)
}

// chatLogQueue decouples handlers from disk writes: one writer goroutine
// owns the files, so lines never interleave and no lock is held during I/O.
var chatLogQueue = make(chan chatEntry, 256)

// recordChat queues an entry without blocking; a full queue drops the
// entry rather than stalling message handling.
func recordChat(en chatEntry) {
	select {
	case chatLogQueue <- en:
	default:
		slog.Warn("chat log queue full, entry dropped", "id", en.ID)
	}
}

// writeChatLog appends queued entries to their chat files. Entries still
// queued when the program exits are lost.
func writeChatLog() {
	if err := os.MkdirAll(chatLogDir, 0o700); err != nil {
		slog.Error("chat log disabled", "err", err)
		return
	}
	for en := range chatLogQueue {
		if err := appendLine(chatLogPath(en.Chat), chatLogLine(en)); err != nil {
			slog.Warn("chat log write failed", "chat", en.Chat, "err", err)
		}
	}
}

// appendLine opens per write; chat volume is low, and it keeps no file
// handles open across reconnects.
func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// sendMessage is the only way the bot sends, so outgoing messages are logged too.
func sendMessage(ctx context.Context, client *whatsmeow.Client, chat types.JID, m *waE2E.Message) (whatsmeow.SendResponse, error) {
	resp, err := client.SendMessage(ctx, chat, m)
	if err == nil {
		recordChat(chatEntry{At: resp.Timestamp, Chat: chat, ID: resp.ID, Who: "bot", Kind: kindOf(m), Text: entryText(m)})
	}
	return resp, err
}

func incomingEntry(evt *events.Message) chatEntry {
	who := evt.Info.PushName + " (" + evt.Info.Sender.ToNonAD().String() + ")"
	if evt.Info.IsFromMe {
		who = "me"
	}
	return chatEntry{At: evt.Info.Timestamp, Chat: evt.Info.Chat, ID: evt.Info.ID, Who: who, Kind: kindOf(evt.Message), Text: entryText(evt.Message)}
}

func cmdChatLog(e Env) error {
	if e.Terminal {
		dir, err := filepath.Abs(chatLogDir)
		if err != nil {
			return err
		}
		return e.Say("Chat logs are in %s\nOpen the folder with: open %q", dir, dir)
	}
	path, err := filepath.Abs(chatLogPath(e.Chat()))
	if err != nil {
		return err
	}
	return e.Say("This chat is saved on the bot's computer at:\n%s", path)
}

// =====================================================================
// 4. Event handling
// =====================================================================

// saveMedia downloads any media in the message into ./downloads.
func saveMedia(ctx context.Context, client *whatsmeow.Client, evt *events.Message) {
	att, ok := mediaOf(evt.Message)
	if !ok {
		return
	}
	mimetype := att.GetMimetype()
	data, err := client.Download(ctx, att)
	if err != nil {
		slog.Warn("media download failed", "id", evt.Info.ID, "err", err)
		return
	}
	path, err := saveFile(evt.Info.ID+extFor(mimetype), data)
	if err != nil {
		slog.Warn("media save failed", "id", evt.Info.ID, "err", err)
		return
	}
	slog.Info("media saved", "id", evt.Info.ID, "path", path)
}

// logPollVote decrypts and prints a vote on one of our polls.
func logPollVote(ctx context.Context, client *whatsmeow.Client, evt *events.Message) {
	vote, err := client.DecryptPollVote(ctx, evt)
	if err != nil {
		slog.Warn("poll vote decrypt failed", "id", evt.Info.ID, "err", err)
		return
	}
	slog.Info("poll vote", "sender", evt.Info.Sender, "options", len(vote.GetSelectedOptions()))
}

// runCommand looks up and runs a command, replying with any error.
func runCommand(env Env, name string) {
	cmd, ok := commands[name]
	if !ok {
		_ = env.Say("Unknown command %q. Try !help", name)
		return
	}
	if err := cmd.Run(env); err != nil {
		slog.Warn("command failed", "command", name, "err", err)
		if sayErr := env.Say("Error: %v", err); sayErr != nil {
			slog.Warn("error reply failed", "command", name, "err", sayErr)
		}
	}
}

// kindOf names the message type, e.g. "conversation", "imageMessage", "protocolMessage".
func kindOf(m *waE2E.Message) string {
	kind := "unknown"
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		if fd.JSONName() == "messageContextInfo" {
			return true // metadata, keep looking
		}
		kind = fd.JSONName()
		return false
	})
	return kind
}

// handleMessage processes one incoming message.
func handleMessage(ctx context.Context, client *whatsmeow.Client, evt *events.Message) {
	// Protocol messages are invisible system messages (key sharing, history sync,
	// edits/deletes from your phone). Nothing for a bot to do with them.
	if evt.Message.GetProtocolMessage() != nil {
		slog.Debug("protocol message ignored", "type", evt.Message.GetProtocolMessage().GetType(), "sender", evt.Info.Sender)
		return
	}

	recordChat(incomingEntry(evt))
	if r := evt.Message.GetReactionMessage(); r != nil {
		slog.Info("reaction", "sender", evt.Info.Sender.ToNonAD(), "target", r.GetKey().GetID(),
			"chat", evt.Info.Chat, "removed", r.GetText() == "")
		return
	}

	text := textOf(evt.Message)
	// Bodies and names stay out of logs; use the recent command to read text.
	slog.Info("message", "id", evt.Info.ID, "chat", evt.Info.Chat, "sender", evt.Info.Sender.ToNonAD(),
		"from_me", evt.Info.IsFromMe, "kind", kindOf(evt.Message))
	if !strings.HasPrefix(text, prefix) {
		remember(evt) // commands aren't remembered, so "last" means the last real message
		autoReact(ctx, client, evt, text)
	}

	// Blue ticks: mark the message as read.
	if err := client.MarkRead(ctx, []types.MessageID{evt.Info.ID}, evt.Info.Timestamp, evt.Info.Chat, evt.Info.Sender); err != nil {
		slog.Warn("mark read failed", "id", evt.Info.ID, "err", err)
	}

	if evt.Message.GetPollUpdateMessage() != nil {
		logPollVote(ctx, client, evt)
		return
	}
	saveMedia(ctx, client, evt)

	if name, args, ok := parseCommand(text); ok {
		runCommand(Env{Ctx: ctx, Client: client, Msg: evt, Args: args}, name)
	}
}

// eventHandler returns the function that receives every whatsmeow event.
func eventHandler(ctx context.Context, client *whatsmeow.Client) func(any) {
	return func(raw any) {
		switch evt := raw.(type) {
		case *events.Message:
			// Run in a goroutine so slow commands (sleeps, uploads) don't block other events.
			go handleMessage(ctx, client, evt)
		case *events.Connected:
			slog.Info("connected", "jid", client.Store.ID)
			if err := client.SendPresence(ctx, types.PresenceAvailable); err != nil {
				slog.Warn("set online failed", "err", err)
			}
		case *events.Disconnected:
			slog.Warn("disconnected, reconnecting")
		case *events.LoggedOut:
			slog.Error("logged out: delete the session file and run again to re-link", "session", dbPath)
		case *events.Receipt:
			slog.Debug("receipt", "type", evt.Type, "ids", evt.MessageIDs, "chat", evt.Chat)
		case *events.Presence:
			slog.Debug("presence", "from", evt.From, "offline", evt.Unavailable)
		case *events.ChatPresence:
			slog.Debug("chat presence", "sender", evt.Sender, "state", evt.State, "chat", evt.Chat)
		case *events.GroupInfo:
			slog.Info("group changed", "group", evt.JID, "by", evt.Sender)
		case *events.JoinedGroup:
			slog.Info("joined group", "group", evt.JID)
		case *events.CallOffer:
			slog.Info("incoming call", "from", evt.From)
		case *events.UndecryptableMessage:
			slog.Warn("undecryptable message, retry requested", "id", evt.Info.ID, "sender", evt.Info.Sender)
		}
	}
}

// =====================================================================
// 5. Main
// =====================================================================

func waitForCtrlC() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
}

// readTerminal lets you type commands in the terminal (e.g. "!help" or "help").
// They run as if you sent them in your own "Message yourself" chat.
func readTerminal(ctx context.Context, client *whatsmeow.Client) {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, prefix) {
			line = prefix + line
		}
		name, args, _ := parseCommand(line)
		me := client.Store.ID.ToNonAD()
		fake := &events.Message{
			Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: me, Sender: me, IsFromMe: true}},
			Message: textMsg(line),
		}
		runCommand(Env{Ctx: ctx, Client: client, Msg: fake, Args: args, Terminal: true}, name)
		fmt.Print("> ")
	}
}

func run(ctx context.Context) error {
	client, err := wa.NewClient(ctx, dbPath)
	if err != nil {
		return err
	}
	go writeChatLog()
	client.AddEventHandler(eventHandler(ctx, client))
	if err := wa.Connect(ctx, client); err != nil {
		return err
	}
	defer client.Disconnect()

	fmt.Println("Bot running. Type a command here (try: help), or send !help on WhatsApp. Ctrl+C to stop.")
	fmt.Print("> ")
	go readTerminal(ctx, client)
	waitForCtrlC()
	return nil
}

// setupLogging sends structured logs to stderr; WABOT_LOG=debug also shows
// receipts, presence and typing events.
func setupLogging() {
	level := slog.LevelInfo
	if os.Getenv("WABOT_LOG") == "debug" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

func main() {
	setupLogging()
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
