// wabot: a single-file WhatsApp bot showing most whatsmeow features,
// written as small functions + a command table.
//
// Run:   go run .
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

	_ "github.com/mattn/go-sqlite3" // SQLite driver (stores your login session)
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	dbPath      = "session.db"
	downloadDir = "downloads"
	prefix      = "!"
)

// =====================================================================
// 1. Setup & login
// =====================================================================

// newClient opens (or creates) the session DB and returns a client.
func newClient(ctx context.Context) (*whatsmeow.Client, error) {
	container, err := sqlstore.New(ctx, "sqlite3", "file:"+dbPath+"?_foreign_keys=on", waLog.Noop)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("get device: %w", err)
	}
	return whatsmeow.NewClient(device, waLog.Stdout("Client", "WARN", true)), nil
}

// connect shows a QR code on first run, otherwise just connects.
func connect(ctx context.Context, client *whatsmeow.Client) error {
	if client.Store.ID != nil {
		return client.Connect()
	}
	qrChan, err := client.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := client.Connect(); err != nil {
		return err
	}
	for evt := range qrChan {
		if evt.Event == "code" {
			fmt.Println("Scan this QR code in WhatsApp > Linked devices:")
			qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
		} else {
			fmt.Println("Login event:", evt.Event)
		}
	}
	return nil
}

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

// phoneToJID turns "+91 98765-43210" into 919876543210@s.whatsapp.net.
func phoneToJID(phone string) types.JID {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, phone)
	return types.NewJID(digits, types.DefaultUserServer)
}

// mediaMime returns the mimetype if the message contains downloadable media.
func mediaMime(m *waE2E.Message) (string, bool) {
	switch {
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetMimetype(), true
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetMimetype(), true
	case m.GetAudioMessage() != nil:
		return m.GetAudioMessage().GetMimetype(), true
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage().GetMimetype(), true
	case m.GetStickerMessage() != nil:
		return m.GetStickerMessage().GetMimetype(), true
	}
	return "", false
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
	resp, err := e.Client.SendMessage(e.Ctx, e.Chat(), m)
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
		"ping":      {"!ping", "reply pong", func(e Env) error { return e.Say("pong 🏓") }},
		"echo":      {"!echo <text>", "repeat your text", cmdEcho},
		"reply":     {"!reply [id] <text>", "quoted reply (to a message ID, the quoted msg, or yours)", cmdReply},
		"react":     {"!react [id|last|#n] [emoji|random]", "react to a message (ID, newest, #n from recent, quoted, or yours)", cmdReact},
		"unreact":   {"!unreact [id|last|#n]", "remove your reaction", cmdUnreact},
		"reactall":  {"!reactall <emoji> [n]", "react to the last n messages (default 5)", cmdReactAll},
		"autoreact": {"!autoreact <word> <emoji> | list | off <word>", "auto-react when a message contains a word", cmdAutoReact},
		"reactto":   {"!reactto <chat> <id> [emoji] [sender]", "react to ANY message by hand (sender defaults to me)", cmdReactTo},
		"info":      {"!info [id]", "show chat/sender/id/key of a message", cmdInfo},
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
	to := phoneToJID(phone)
	if _, err := e.Client.SendMessage(e.Ctx, to, textMsg(text)); err != nil {
		return err
	}
	return e.Say("Sent to %s ✅", to.User)
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

// target is the message a react/reply should act on.
type target struct {
	Chat, Sender types.JID
	ID           types.MessageID
	Message      *waE2E.Message
}

// pickTarget decides which message react/reply acts on and returns the leftover args:
//  1. first arg is a known message ID  -> that message (works from terminal too)
//  2. command was sent as a WhatsApp reply to a message -> the quoted message
//  3. otherwise -> the command message itself (WhatsApp only)
func pickTarget(e Env) (target, string, error) {
	first, rest, _ := strings.Cut(e.Args, " ")
	evt, ok := lookup(first)
	if !ok {
		evt, ok = byPosition(first)
	}
	if ok {
		return target{evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Message}, strings.TrimSpace(rest), nil
	}
	if ci := e.Msg.Message.GetExtendedTextMessage().GetContextInfo(); ci.GetStanzaID() != "" {
		sender, err := types.ParseJID(ci.GetParticipant())
		if err != nil || sender.IsEmpty() {
			sender = e.Chat() // private chat: the other person
		}
		return target{e.Chat(), sender, ci.GetStanzaID(), ci.GetQuotedMessage()}, e.Args, nil
	}
	if e.Terminal {
		return target{}, "", errors.New("say which message: react <id|last|#n> 🏓 (see: recent)")
	}
	return target{e.Chat(), e.Msg.Info.Sender, e.Msg.Info.ID, e.Msg.Message}, e.Args, nil
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

var randomEmojis = []string{"😂", "❤️", "🔥", "👍", "😮", "😢", "🙏", "🎉", "💯", "🏓", "😎", "🤯"}

// resolveEmoji turns "" into 👍 and "random" into a random emoji.
func resolveEmoji(s string) string {
	switch s {
	case "":
		return "👍"
	case "random":
		return randomEmojis[time.Now().UnixNano()%int64(len(randomEmojis))]
	}
	return s
}

// sendReaction is the one place that actually sends a reaction ("" removes it).
func sendReaction(ctx context.Context, client *whatsmeow.Client, t target, emoji string) error {
	_, err := client.SendMessage(ctx, t.Chat, client.BuildReaction(t.Chat, t.Sender, t.ID, emoji))
	return err
}

func targetOf(evt *events.Message) target {
	return target{evt.Info.Chat, evt.Info.Sender, evt.Info.ID, evt.Message}
}

func cmdUnreact(e Env) error {
	t, _, err := pickTarget(e)
	if err != nil {
		return err
	}
	if err := sendReaction(e.Ctx, e.Client, t, ""); err != nil {
		return err
	}
	return e.Say("Reaction removed from %s ✅", t.ID)
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
	var picks []target
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
	return e.Say("Reacted to %d message(s) ✅", len(picks))
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
			return e.Say("No auto-react rules. Add one: autoreact hello 👋")
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
		return e.Say("Will react %s to messages containing %q ✅", f[1], f[0])
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
	if emoji != "" {
		_ = sendReaction(ctx, client, targetOf(evt), resolveEmoji(emoji))
		fmt.Printf("  auto-reacted %s\n", emoji)
	}
}

// describeTarget shows the 4 BuildReaction inputs and the key WhatsApp will receive.
func describeTarget(e Env, t target, emoji string) string {
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

// toJID accepts a full JID (…@lid, …@g.us, …@s.whatsapp.net) or a plain phone number.
func toJID(s string) (types.JID, error) {
	if strings.Contains(s, "@") {
		return types.ParseJID(s)
	}
	return phoneToJID(s), nil
}

// cmdReactTo reacts using all 4 inputs typed by hand — no remembered message needed.
//
//	reactto 278833789944039@lid 3EB0AC684A7EE08CD8C216 🏓          (my own message)
//	reactto 120363…@g.us 3A88EC035EDE6284E0F0 🔥 919876543210      (someone else's, in a group)
func cmdReactTo(e Env) error {
	f := strings.Fields(e.Args)
	if len(f) < 2 {
		return errors.New("usage: reactto <chat> <id> [emoji] [sender|me]")
	}
	chat, err := toJID(f[0])
	if err != nil {
		return fmt.Errorf("bad chat: %w", err)
	}
	t := target{Chat: chat, ID: strings.ToUpper(f[1]), Sender: *e.Client.Store.ID} // default: my message
	emoji := "👍"
	if len(f) >= 3 {
		emoji = f[2]
	}
	if len(f) >= 4 && f[3] != "me" {
		if t.Sender, err = toJID(f[3]); err != nil {
			return fmt.Errorf("bad sender: %w", err)
		}
	}
	if _, err := e.Client.SendMessage(e.Ctx, t.Chat, e.Client.BuildReaction(t.Chat, t.Sender, t.ID, emoji)); err != nil {
		return err
	}
	return e.Say("%s\nReacted ✅", describeTarget(e, t, emoji))
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
		text = "This is a quoted reply ↩️"
	}
	msg := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String(text),
		ContextInfo: &waE2E.ContextInfo{
			StanzaID:      proto.String(t.ID),
			Participant:   proto.String(t.Sender.ToNonAD().String()),
			QuotedMessage: t.Message,
		},
	}}
	if _, err := e.Client.SendMessage(e.Ctx, t.Chat, msg); err != nil {
		return err
	}
	if e.Terminal {
		return e.Say("Replied to %s ✅", t.ID)
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
		return e.Say("%s\nReacted ✅", describeTarget(e, t, emoji))
	}
	return nil
}

func cmdEdit(e Env) error {
	id, err := e.Send(textMsg("Original text… (will be edited in 2s)"))
	if err != nil {
		return err
	}
	time.Sleep(2 * time.Second)
	_, err = e.Send(e.Client.BuildEdit(e.Chat(), id, textMsg("✏️ Edited text!")))
	return err
}

func cmdDelete(e Env) error {
	id, err := e.Send(textMsg("This message will self-destruct in 3s 💣"))
	if err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	// Sender = own JID (empty JID also means "me").
	_, err = e.Send(e.Client.BuildRevoke(e.Chat(), types.EmptyJID, id))
	return err
}

func cmdTyping(e Env) error {
	_ = e.Client.SendChatPresence(e.Ctx, e.Chat(), types.ChatPresenceComposing, types.ChatPresenceMediaText)
	time.Sleep(3 * time.Second)
	_ = e.Client.SendChatPresence(e.Ctx, e.Chat(), types.ChatPresencePaused, types.ChatPresenceMediaText)
	return e.Say("Done typing ⌨️")
}

func cmdImage(e Env) error {
	data := demoPNG()
	up, err := e.Client.Upload(e.Ctx, data, whatsmeow.MediaImage)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	_, err = e.Send(imageMsg(up, "image/png", "Generated by wabot 🎨"))
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
	res, err := e.Client.IsOnWhatsApp(e.Ctx, []string{"+" + phoneToJID(e.Args).User})
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, r := range res {
		fmt.Fprintf(&b, "%s → on WhatsApp: %v (%s)\n", r.Query, r.IsIn, r.JID)
	}
	return e.Say("%s", b.String())
}

// profile is what WhatsApp tells us about a user.
type profile struct {
	JID        types.JID
	About      string
	Devices    int
	PictureURL string // empty when the picture is hidden or not set
}

// whoisTarget picks whose profile to show: the argument if given, else the sender.
func whoisTarget(e Env) (types.JID, error) {
	if e.Args == "" {
		return e.Msg.Info.Sender.ToNonAD(), nil
	}
	return toJID(e.Args)
}

// lookupProfile gathers About, devices and picture URL in one place so
// commands never call the user APIs directly.
func lookupProfile(ctx context.Context, client *whatsmeow.Client, who types.JID) (profile, error) {
	infos, err := client.GetUserInfo(ctx, []types.JID{who})
	if err != nil {
		return profile{}, fmt.Errorf("user info %s: %w", who, err)
	}
	info := infos[who]
	p := profile{JID: who, About: info.Status, Devices: len(info.Devices)}
	// A missing picture is normal (privacy settings), so it is not an error.
	if pic, err := client.GetProfilePictureInfo(ctx, who, &whatsmeow.GetProfilePictureParams{}); err == nil && pic != nil {
		p.PictureURL = pic.URL
	}
	return p, nil
}

// profileText renders a profile for chat or terminal.
func profileText(p profile) string {
	pic := "hidden or none"
	if p.PictureURL != "" {
		pic = "available"
	}
	return fmt.Sprintf("*%s*\nAbout: %s\nDevices: %d\nProfile pic: %s", p.JID, p.About, p.Devices, pic)
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
	defer resp.Body.Close()
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
func sendProfilePic(e Env, p profile) error {
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
	p, err := lookupProfile(e.Ctx, e.Client, who)
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
	p, err := lookupProfile(e.Ctx, e.Client, who)
	if err != nil {
		return err
	}
	return sendProfilePic(e, p)
}

func cmdStatus(e Env) error {
	if e.Args == "" {
		return errors.New("usage: !status <text>")
	}
	if err := e.Client.SetStatusMessage(e.Ctx, types.SetStatusInput{Text: proto.String(e.Args)}); err != nil {
		return err
	}
	return e.Say("About updated ✅")
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
// 4. Event handling
// =====================================================================

// saveMedia downloads any media in the message into ./downloads.
func saveMedia(ctx context.Context, client *whatsmeow.Client, evt *events.Message) {
	mimetype, ok := mediaMime(evt.Message)
	if !ok {
		return
	}
	data, err := client.DownloadAny(ctx, evt.Message)
	if err != nil {
		fmt.Println("  download failed:", err)
		return
	}
	path, err := saveFile(evt.Info.ID+extFor(mimetype), data)
	if err != nil {
		fmt.Println("  save failed:", err)
		return
	}
	fmt.Println("  saved media →", path)
}

// logPollVote decrypts and prints a vote on one of our polls.
func logPollVote(ctx context.Context, client *whatsmeow.Client, evt *events.Message) {
	vote, err := client.DecryptPollVote(ctx, evt)
	if err != nil {
		fmt.Println("  poll vote decrypt failed:", err)
		return
	}
	fmt.Printf("  poll vote from %s: %d option(s) selected (hashes)\n",
		evt.Info.Sender, len(vote.GetSelectedOptions()))
}

// runCommand looks up and runs a command, replying with any error.
func runCommand(env Env, name string) {
	cmd, ok := commands[name]
	if !ok {
		_ = env.Say("Unknown command %q. Try !help", name)
		return
	}
	if err := cmd.Run(env); err != nil {
		_ = env.Say("⚠️ %v", err)
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
		fmt.Printf("[sys] %s from %s (internal sync, ignored)\n",
			evt.Message.GetProtocolMessage().GetType(), evt.Info.Sender)
		return
	}

	if r := evt.Message.GetReactionMessage(); r != nil {
		what := "reacted " + r.GetText()
		if r.GetText() == "" {
			what = "removed their reaction"
		}
		fmt.Printf("[reaction] %s (%s) %s on message %s in %s\n",
			evt.Info.PushName, evt.Info.Sender.ToNonAD(), what, r.GetKey().GetID(), evt.Info.Chat)
		return
	}

	text := textOf(evt.Message)
	from := evt.Info.PushName
	if evt.Info.IsFromMe {
		from = "me"
	}
	fmt.Printf("[msg] id=%s %s | %s (%s): %s\n", evt.Info.ID, evt.Info.Chat, from, kindOf(evt.Message), text)
	if !strings.HasPrefix(text, prefix) {
		remember(evt) // commands aren't remembered, so "last" means the last real message
		autoReact(ctx, client, evt, text)
	}

	// Blue ticks: mark the message as read.
	_ = client.MarkRead(ctx, []types.MessageID{evt.Info.ID}, evt.Info.Timestamp, evt.Info.Chat, evt.Info.Sender)

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
			fmt.Println("[conn] connected as", client.Store.ID)
			_ = client.SendPresence(ctx, types.PresenceAvailable) // show "online"
		case *events.Disconnected:
			fmt.Println("[conn] disconnected (auto-reconnecting)")
		case *events.LoggedOut:
			fmt.Println("[conn] logged out — delete", dbPath, "and run again to re-link")
		case *events.Receipt:
			fmt.Printf("[receipt] %s %v in %s\n", evt.Type, evt.MessageIDs, evt.Chat)
		case *events.Presence:
			fmt.Printf("[presence] %s offline=%v\n", evt.From, evt.Unavailable)
		case *events.ChatPresence:
			fmt.Printf("[typing] %s %s in %s\n", evt.Sender, evt.State, evt.Chat)
		case *events.GroupInfo:
			fmt.Printf("[group] change in %s by %s\n", evt.JID, evt.Sender)
		case *events.JoinedGroup:
			fmt.Printf("[group] joined %s (%s)\n", evt.Name, evt.JID)
		case *events.CallOffer:
			fmt.Printf("[call] incoming call from %s\n", evt.From)
		case *events.UndecryptableMessage:
			fmt.Printf("[msg] couldn't decrypt message from %s (will retry)\n", evt.Info.Sender)
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
	client, err := newClient(ctx)
	if err != nil {
		return err
	}
	client.AddEventHandler(eventHandler(ctx, client))
	if err := connect(ctx, client); err != nil {
		return err
	}
	defer client.Disconnect()

	fmt.Println("Bot running. Type a command here (try: help), or send !help on WhatsApp. Ctrl+C to stop.")
	fmt.Print("> ")
	go readTerminal(ctx, client)
	waitForCtrlC()
	return nil
}

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
