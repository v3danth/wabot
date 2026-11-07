package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
	"wabot/wa"

	"go.mau.fi/whatsmeow"
)

const (
	defaultAPIAddr = "127.0.0.1:8080" // localhost only: the API sends as your account
	maxTextLen     = 4096
	flowTimeout    = 60 * time.Second
)

// flowRequest is the JSON body of POST /flow.
type flowRequest struct {
	Phone string `json:"phone"`
	Text  string `json:"text"` // accepted for compatibility; the flow does not send it
}

// flowResponse is what POST /flow returns on success.
type flowResponse struct {
	JID        string       `json:"jid"`
	Name       string       `json:"name"`
	NameSource string       `json:"name_source,omitempty"`
	About      string       `json:"about"`
	Devices    int          `json:"devices"`
	PictureURL string       `json:"picture_url,omitempty"`
	PhotoPath  string       `json:"photo_path,omitempty"`
	MessageID  string       `json:"message_id"`
	Reaction   reactionInfo `json:"reaction"`
	Timings    []stepMillis `json:"timings"`
	TotalMS    int64        `json:"total_ms"`
}

// reactionInfo says whether WhatsApp accepted the reaction. Sent is true only
// after the server ack; it cannot tell whether message_id really exists.
type reactionInfo struct {
	Emoji      string `json:"emoji"`
	Sent       bool   `json:"sent"`
	ID         string `json:"id,omitempty"`
	ServerTime string `json:"server_time,omitempty"`
}

type stepMillis struct {
	Step string `json:"step"`
	MS   int64  `json:"ms"`
}

// flowFunc is what the handler runs per request; it is a value so the
// handler can be tested without a WhatsApp connection.
type flowFunc func(ctx context.Context, phone, text string) (wa.FlowState, error)

// startAPI serves POST /flow and returns a function that shuts it down.
func startAPI(client *whatsmeow.Client) (stop func(), err error) {
	addr := os.Getenv("WABOT_API_ADDR")
	if addr == "" {
		addr = defaultAPIAddr
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	flow := func(ctx context.Context, phone, _ string) (wa.FlowState, error) {
		return wa.ContactFlow(ctx, client, phone)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /flow", flowHandler(flow))
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      flowTimeout + 10*time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP API stopped", "err", err)
		}
	}()
	slog.Info("HTTP API listening", "addr", ln.Addr().String())
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			slog.Warn("HTTP API shutdown", "err", err)
		}
	}, nil
}

// flowHandler reads {"phone","text"}, runs whois -> react, and
// replies with the result or {"error": "..."}.
func flowHandler(flow flowFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req flowRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
			return
		}
		phone, err := normalizePhone(req.Phone)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		text := strings.TrimSpace(req.Text)
		if text == "" {
			text = "Hello from wabot"
		}
		if len(text) > maxTextLen {
			writeError(w, http.StatusBadRequest, fmt.Errorf("text longer than %d bytes", maxTextLen))
			return
		}

		// Bounds the WhatsApp calls so a stalled request cannot hang forever.
		ctx, cancel := context.WithTimeout(r.Context(), flowTimeout)
		defer cancel()
		st, err := flow(ctx, phone, text)
		if err != nil {
			slog.Warn("flow failed", "err", err)
			writeError(w, flowErrorStatus(err), err)
			return
		}
		writeJSON(w, http.StatusOK, flowResponseOf(st))
	}
}

// flowErrorStatus tells the caller whether retrying could help.
func flowErrorStatus(err error) int {
	switch {
	case errors.Is(err, wa.ErrNotOnWhatsApp):
		return http.StatusUnprocessableEntity
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}

// normalizePhone keeps the digits of an international number and checks the
// E.164 length (8 to 15 digits with country code).
func normalizePhone(s string) (string, error) {
	digits := wa.PhoneToJID(s).User
	if len(digits) < 8 || len(digits) > 15 {
		return "", errors.New("phone must be 8-15 digits including country code, e.g. 919876543210")
	}
	return digits, nil
}

func flowResponseOf(st wa.FlowState) flowResponse {
	timings := make([]stepMillis, 0, len(st.Timings))
	var total time.Duration
	for _, t := range st.Timings {
		timings = append(timings, stepMillis{t.Step, t.Took.Milliseconds()})
		total += t.Took
	}
	reaction := reactionInfo{Emoji: st.Emoji, Sent: st.Reaction.ID != "", ID: st.Reaction.ID}
	if !st.Reaction.ServerTime.IsZero() {
		reaction.ServerTime = st.Reaction.ServerTime.UTC().Format(time.RFC3339)
	}
	return flowResponse{
		JID:        st.Who.String(),
		Name:       st.Profile.Name,
		NameSource: st.Profile.NameSource,
		About:      st.Profile.About,
		Devices:    st.Profile.Devices,
		PictureURL: st.Profile.PictureURL,
		PhotoPath:  st.PhotoPath,
		MessageID:  st.MessageID,
		Reaction:   reaction,
		Timings:    timings,
		TotalMS:    total.Milliseconds(),
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write response", "err", err)
	}
}
