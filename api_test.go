package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

const testToken = "secret"

func connectedTrue() bool { return true }

// fakeFlow stands in for contactFlow and records what it was called with.
func fakeFlow(err error, gotPhone *string) flowFunc {
	return func(_ context.Context, phone, text string) (flowState, error) {
		*gotPhone = phone
		if err != nil {
			return flowState{}, err
		}
		return flowState{
			Who:     types.NewJID(phone, types.DefaultUserServer),
			Profile: profile{About: "busy", Devices: 2},
			Text:    text,
			Emoji:   defaultReaction,
			SentID:  "3EB0TEST",
		}, nil
	}
}

func postFlow(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/flow", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestFlowSuccess(t *testing.T) {
	var gotPhone string
	h := newAPIHandler(testToken, connectedTrue, fakeFlow(nil, &gotPhone))
	rec := postFlow(t, h, testToken, `{"phone":"+91 98864-73508","text":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if gotPhone != "919886473508" {
		t.Errorf("flow got phone %q, want digits only", gotPhone)
	}
	var resp flowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.MessageID != "3EB0TEST" || resp.About != "busy" || resp.JID != "919886473508@s.whatsapp.net" {
		t.Errorf("response = %+v", resp)
	}
}

func TestFlowStatusCodes(t *testing.T) {
	tests := []struct {
		name, token, body string
		flowErr           error
		connected         bool
		want              int
	}{
		{"no token", "", `{"phone":"919886473508"}`, nil, true, http.StatusUnauthorized},
		{"wrong token", "nope", `{"phone":"919886473508"}`, nil, true, http.StatusUnauthorized},
		{"bad json", testToken, `{`, nil, true, http.StatusBadRequest},
		{"unknown field", testToken, `{"phone":"919886473508","x":1}`, nil, true, http.StatusBadRequest},
		{"short phone", testToken, `{"phone":"123"}`, nil, true, http.StatusBadRequest},
		{"long text", testToken, fmt.Sprintf(`{"phone":"919886473508","text":%q}`, strings.Repeat("a", maxTextLen+1)), nil, true, http.StatusBadRequest},
		{"disconnected", testToken, `{"phone":"919886473508"}`, nil, false, http.StatusServiceUnavailable},
		{"not on whatsapp", testToken, `{"phone":"919886473508"}`, fmt.Errorf("step 1: %w", errNotOnWhatsApp), true, http.StatusUnprocessableEntity},
		{"timeout", testToken, `{"phone":"919886473508"}`, context.DeadlineExceeded, true, http.StatusGatewayTimeout},
		{"upstream error", testToken, `{"phone":"919886473508"}`, errors.New("boom"), true, http.StatusBadGateway},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPhone string
			h := newAPIHandler(testToken, func() bool { return tc.connected }, fakeFlow(tc.flowErr, &gotPhone))
			if rec := postFlow(t, h, tc.token, tc.body); rec.Code != tc.want {
				t.Errorf("status %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestFlowRejectsGet(t *testing.T) {
	var gotPhone string
	h := newAPIHandler(testToken, connectedTrue, fakeFlow(nil, &gotPhone))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/flow", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /flow status %d, want 405", rec.Code)
	}
}

func TestHealthz(t *testing.T) {
	for connected, want := range map[bool]int{true: http.StatusOK, false: http.StatusServiceUnavailable} {
		var gotPhone string
		h := newAPIHandler(testToken, func() bool { return connected }, fakeFlow(nil, &gotPhone))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != want {
			t.Errorf("connected=%v: status %d, want %d", connected, rec.Code, want)
		}
	}
}

func TestRecoverTurnsPanicInto500(t *testing.T) {
	h := withRecover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", rec.Code)
	}
}
