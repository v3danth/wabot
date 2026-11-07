package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
)

const (
	defaultAPIAddr = "127.0.0.1:8080" // localhost only: the API sends as your account
	maxBodyBytes   = 8 << 10
	maxTextLen     = 4096
	flowTimeout    = 60 * time.Second
)

// flowRequest is the JSON body of POST /flow.
type flowRequest struct {
	Phone string `json:"phone"`
	Text  string `json:"text"`
}

// flowResponse is what POST /flow returns on success.
type flowResponse struct {
	JID        string `json:"jid"`
	About      string `json:"about"`
	Devices    int    `json:"devices"`
	PictureURL string `json:"picture_url,omitempty"`
	MessageID  string `json:"message_id"`
	Reaction   string `json:"reaction"`
}

// errorResponse is the JSON body of every non-2xx reply.
type errorResponse struct {
	Error string `json:"error"`
}

// flowFunc is contactFlow's shape; handlers take it as a value so they can
// be tested without a WhatsApp connection.
type flowFunc func(ctx context.Context, phone, text string) (flowState, error)

// startAPI serves the HTTP API when WABOT_API_TOKEN is set and returns a
// function that shuts it down. Without a token the API stays off, because
// anyone who can reach it can message people as you.
func startAPI(client *whatsmeow.Client) (stop func(), err error) {
	token := os.Getenv("WABOT_API_TOKEN")
	if token == "" {
		slog.Info("HTTP API off; set WABOT_API_TOKEN to enable it")
		return func() {}, nil
	}
	addr := os.Getenv("WABOT_API_ADDR")
	if addr == "" {
		addr = defaultAPIAddr
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	flow := func(ctx context.Context, phone, text string) (flowState, error) {
		return contactFlow(ctx, client, phone, text)
	}
	srv := &http.Server{
		Handler:           newAPIHandler(token, client.IsConnected, flow),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
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

// newAPIHandler builds the routes. Cross-cutting concerns are wrappers,
// applied in one place: recover outermost, then logging, then auth.
func newAPIHandler(token string, connected func() bool, flow flowFunc) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler(connected))
	mux.Handle("POST /flow", withAuth(token, withTimeout(flowTimeout, flowHandler(connected, flow))))
	return withRecover(withRequestLog(mux))
}

func healthHandler(connected func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if !connected() {
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{"not connected to WhatsApp"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// flowHandler validates the request, runs whois -> send -> react, and maps
// failures to status codes the caller can act on.
func flowHandler(connected func() bool, flow flowFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := decodeFlowRequest(w, r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{err.Error()})
			return
		}
		if !connected() {
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{"not connected to WhatsApp"})
			return
		}
		st, err := flow(r.Context(), req.Phone, req.Text)
		switch {
		case errors.Is(err, errNotOnWhatsApp):
			writeJSON(w, http.StatusUnprocessableEntity, errorResponse{err.Error()})
		case errors.Is(err, context.DeadlineExceeded):
			writeJSON(w, http.StatusGatewayTimeout, errorResponse{err.Error()})
		case err != nil:
			slog.Warn("flow failed", "err", err)
			writeJSON(w, http.StatusBadGateway, errorResponse{err.Error()})
		default:
			writeJSON(w, http.StatusOK, flowResponseOf(st))
		}
	}
}

// decodeFlowRequest reads and validates the body; everything past this
// point can trust the phone and text.
func decodeFlowRequest(w http.ResponseWriter, r *http.Request) (flowRequest, error) {
	var req flowRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, fmt.Errorf("invalid JSON: %w", err)
	}
	phone, err := normalizePhone(req.Phone)
	if err != nil {
		return req, err
	}
	req.Phone = phone
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		req.Text = "Hello from wabot"
	}
	if len(req.Text) > maxTextLen {
		return req, fmt.Errorf("text longer than %d bytes", maxTextLen)
	}
	return req, nil
}

// normalizePhone keeps the digits of an international number and checks the
// E.164 length (8 to 15 digits with country code).
func normalizePhone(s string) (string, error) {
	digits := phoneToJID(s).User
	if len(digits) < 8 || len(digits) > 15 {
		return "", errors.New("phone must be 8-15 digits including country code, e.g. 919876543210")
	}
	return digits, nil
}

func flowResponseOf(st flowState) flowResponse {
	return flowResponse{
		JID:        st.Who.String(),
		About:      st.Profile.About,
		Devices:    st.Profile.Devices,
		PictureURL: st.Profile.PictureURL,
		MessageID:  st.SentID,
		Reaction:   st.Emoji,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write response", "err", err)
	}
}

// ---- middleware ----

// withAuth requires "Authorization: Bearer <token>". The comparison is
// constant time so the token cannot be guessed byte by byte from timings.
func withAuth(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			writeJSON(w, http.StatusUnauthorized, errorResponse{"missing or wrong bearer token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withTimeout bounds how long one request may keep WhatsApp calls running.
func withTimeout(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withRecover turns a panic in one request into a 500 instead of killing the bot.
func withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.Error("panic in HTTP handler", "path", r.URL.Path, "panic", v)
				writeJSON(w, http.StatusInternalServerError, errorResponse{"internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder remembers the status code for the request log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// withRequestLog logs one line per request; bodies are not logged because
// they hold phone numbers and message text.
func withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.Info("http request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "took", time.Since(start))
	})
}
