# wabot

A small WhatsApp toolkit built on [whatsmeow](https://github.com/tulir/whatsmeow).
It links to your WhatsApp account the same way WhatsApp Web does (by scanning a
QR code), then lets you control that account from code.

It contains two programs:

| Program | What it is | Start it with |
|---|---|---|
| **API** | A tiny web server. You send it a phone number; it looks up that contact on WhatsApp and reacts to a message. | `make api` |
| **Bot** | An interactive bot. You type commands (like `help`, `whois`, `react`) in the terminal or send them in WhatsApp. | `make bot` |

> **Heads up:** everything here acts **as you**, from your real WhatsApp number.
> Try it on your own number or a friend's first. WhatsApp can ban numbers that
> behave like spam bots, so don't send in bulk.

---

## 1. One-time setup

You need four things installed: **Go**, **Git**, a **C compiler**, and **make**.
The commands below are for macOS. Run them in the Terminal app.

### 1.1 Install the tools

```sh
# C compiler + make + git (Apple's command line tools). A popup opens; click Install.
xcode-select --install

# Homebrew (a package installer for macOS). Skip if `brew --version` already works.
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"

# Go
brew install go
```

Check that everything is there. Each command should print a version, not "command not found":

```sh
go version      # needs go1.26 or newer
git --version
make --version
cc --version
```

> **Why a C compiler?** Your login is saved in a small SQLite database, and the
> SQLite library used here is written in C.

### 1.2 Get the code

wabot uses a local copy of whatsmeow, and expects it **in the folder next to it**:

```
~/Projects/
├── whatsmeow/   <- the library
└── wabot/       <- this project
```

```sh
mkdir -p ~/Projects && cd ~/Projects
git clone https://github.com/tulir/whatsmeow.git
git clone <your-wabot-repo-url> wabot
cd wabot
```

If wabot is already on your computer, just `cd ~/Projects/wabot`.

### 1.3 Download dependencies and check it builds

```sh
go mod download
make test
```

You should see three `ok` lines. That means the setup works. The tests don't
need a WhatsApp login.

---

## 2. Log in (first run only)

```sh
make api
```

1. A big QR code appears in the terminal.
2. On your phone, open **WhatsApp > Settings > Linked devices > Link a device**.
3. Scan the QR code.

When it worked, you'll see:

```
INFO connected jid=919886473508:4@s.whatsapp.net
INFO HTTP API listening addr=127.0.0.1:8080
```

Your login is saved in `session.db` in this folder, so **next time there's no QR
code**: it connects straight away.

> `session.db` is the key to your WhatsApp account. Never share it or commit it
> (it's already in `.gitignore`). To log out, unlink the device on your phone
> or delete `session.db`.

---

## 3. Use the API

### 3.1 Start it

```sh
make api
```

Leave this terminal open; the API runs as long as it does. Press **Ctrl+C** to stop it.

### 3.2 Send a request

Open a **second** terminal window and run:

```sh
curl -X POST localhost:8080/flow -d '{"phone": "918250053558"}'
```

- `phone` is the person's number **with country code**, e.g. India `91…`, US `1…`.
  Spaces, `+` and `-` are fine: `"+91 82500-53558"` works too.
- `localhost:8080` means "this computer, port 8080". Only programs on your own
  computer can reach it.

### 3.3 What happens

The request runs five steps, in order, and stops at the first one that fails:

1. **check registered**: is this number on WhatsApp?
2. **whois**: fetch their name, About text, number of devices and profile picture link.
3. **presence**: are they online right now? Waits up to 5 seconds for WhatsApp
   to say. `status` is `online`, `offline` or `unknown`; `last_seen` appears only
   if their privacy settings share it.
4. **save photo**: download the profile picture to `profile_<phone>.jpg` in a temp
   folder (skipped if the picture is hidden). The path is in `photo_path`.
5. **react**: put a 👍 on the message set in `ReactionMessageID` (see 3.5).

### 3.4 The answer

Success (HTTP 200):

```json
{
  "jid": "918250053558@s.whatsapp.net",
  "about": "Hey there! I am using WhatsApp.",
  "devices": 2,
  "picture_url": "https://pps.whatsapp.net/...",
  "status": "offline",
  "last_seen": "2026-11-07T13:52:10Z",
  "photo_path": "/var/folders/.../T/wabot-photos/profile_918250053558.jpg",
  "message_id": "3EB0AC684A7EE08CD8C216",
  "reaction": {"emoji": "👍", "sent": true, "id": "3EB07C...", "server_time": "2026-11-07T14:16:31Z"},
  "timings": [{"step": "check registered", "ms": 211}, {"step": "whois", "ms": 414},
              {"step": "save photo", "ms": 150}, {"step": "react", "ms": 696}],
  "total_ms": 1471
}
```

Open the photo with `open "<photo_path>"`. `name` and `about` are empty when
WhatsApp doesn't share them (their privacy settings, or you've never chatted).

Errors always look like `{"error": "..."}`, with one of these status codes:

| Code | Meaning | What to do |
|---|---|---|
| 400 | Bad request: broken JSON, or the phone isn't 8-15 digits | Fix the request body |
| 422 | That number isn't on WhatsApp | Check the number and country code |
| 502 | WhatsApp returned an error; the message says which step | Read the error; retry |
| 504 | It took longer than 60 seconds | Retry; check your internet |

To see the status code too, add `-i` to curl: `curl -i -X POST ...`

### 3.5 Choose the message to react to

The reaction goes on one fixed message, set in `wa/flow.go`:

```go
const ReactionMessageID types.MessageID = "3EB0TEST"
```

`3EB0TEST` is a placeholder, so out of the box the reaction won't land on
anything. To use a real message:

1. Stop the API (Ctrl+C) and start the bot: `make bot`.
2. From your phone, send any message to the person.
3. The bot's log shows a line like:
   ```
   level=INFO msg=message id=3EB0AC684A7EE08CD8C216 chat=918250053558@s.whatsapp.net ... from_me=true
   ```
4. Copy the `id` value into `ReactionMessageID`, save, stop the bot, and run `make api` again.

It must be a message **you** sent, in the chat with the number you pass to the API.

---

## 4. Use the bot

```sh
make bot
```

Then type commands in the terminal at the `>` prompt (try `help`), or send them
with a `!` in WhatsApp, e.g. `!help` in your "Message yourself" chat.

A few useful ones:

| Command | What it does |
|---|---|
| `help` | List every command |
| `whois 918250053558` | About text, devices and profile picture |
| `flow 918250053558` | The same steps as the API |
| `send 918250053558 hi` | Send a text |
| `recent` | The last messages the bot saw, with their IDs |
| `react last 🔥` | React to the newest message |
| `chatlog` | Where chats are saved (the `chats/` folder) |

Received photos, videos and files are saved to `downloads/`.

> **Run one program at a time.** The API and the bot share the same login, and
> WhatsApp disconnects one of them if both are connected. Stop one (Ctrl+C)
> before starting the other.

---

## 5. All commands

Run these from the `wabot` folder. `make help` lists them too.

| Command | What it does |
|---|---|
| `make api` | Start the API on `127.0.0.1:8080` |
| `make bot` | Start the interactive bot |
| `make test` | Run all tests (no login needed) |
| `make lint` | Check formatting and common mistakes (needs [golangci-lint](https://golangci-lint.run/welcome/install/)) |
| `make build` | Build both programs into `bin/` |
| `make clean` | Delete `bin/` (keeps your login and chats) |

Settings, passed in front of the command:

| Setting | Example | Effect |
|---|---|---|
| `WABOT_API_ADDR` | `WABOT_API_ADDR=127.0.0.1:9000 make api` | Use another port |
| `WABOT_PHOTO_DIR` | `WABOT_PHOTO_DIR=~/Pictures/wabot make api` | Save profile photos there instead of the temp folder |
| `WABOT_LOG=debug` | `WABOT_LOG=debug make bot` | Bot also logs receipts, typing and presence |

---

## 6. How the code is organised

```
wabot/
├── wa/              shared WhatsApp code, used by both programs
│   ├── client.go      log in (QR code) and connect
│   ├── jid.go         phone number -> WhatsApp ID
│   ├── profile.go     whois: name, About, devices, picture
│   ├── presence.go    online / offline / last seen
│   ├── photo.go       download and save the profile picture
│   ├── react.go       send a reaction
│   └── flow.go        the API's steps: check -> whois -> presence -> save photo -> react
├── cmd/api/         the API program
│   ├── main.go        connect, start the server, wait for Ctrl+C
│   └── api.go         the POST /flow handler
├── cmd/bot/         the interactive bot (all commands, chat log, downloads)
├── Makefile         the commands above
└── session.db       your login (created on first run, never commit it)
```

To follow one API request through the code: `cmd/api/api.go` `flowHandler` ->
`wa/flow.go` `ContactFlow` -> `RunSteps` -> each step in `ContactFlowSteps`.

To add a step to the API, write a function with the same shape as
`fetchProfile` in `wa/flow.go` and add it to `ContactFlowSteps`.

---

## 7. Troubleshooting

| Problem | Fix |
|---|---|
| `go: command not found` | Install Go (step 1.1), then open a new terminal window. |
| `cgo: C compiler "cc" not found` | Run `xcode-select --install`. |
| `replacement directory ../whatsmeow does not exist` | Clone whatsmeow **next to** wabot (step 1.2). |
| `listen 127.0.0.1:8080: address already in use` | Something else is on port 8080, probably an old API still running. Stop it with Ctrl+C in its terminal, or find it with `lsof -i :8080` and `kill <PID>`. Or use another port: `WABOT_API_ADDR=127.0.0.1:9000 make api`. |
| `curl: (7) Failed to connect` | The API isn't running. Start `make api` in another terminal first. |
| The QR code shows every time | You started it from a different folder. Always run from the `wabot` folder, where `session.db` lives. |
| It keeps disconnecting | The API and the bot are both running. Stop one. |
| Logged out, or the phone unlinked it | Delete `session.db` and run again to scan a new QR code. |
| HTTP 200 but no reaction appears | `ReactionMessageID` is still the placeholder, or isn't a message **you** sent in that chat (step 3.5). |
| Profile picture link won't open | The links expire after about 10 days. Call the API again for a fresh one. |
