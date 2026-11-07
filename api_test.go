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

// fakeFlow stands in for contactFlow and records the phone it was called with.
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

func postFlow(h http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/flow", strings.NewReader(body)))
	return rec
}

func TestFlowSuccess(t *testing.T) {
	var gotPhone string
	rec := postFlow(flowHandler(fakeFlow(nil, &gotPhone)), `{"phone":"+91 98864-73508","text":"hi"}`)
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
	const ok = `{"phone":"919886473508"}`
	tests := []struct {
		name, body string
		flowErr    error
		want       int
	}{
		{"bad json", `{`, nil, http.StatusBadRequest},
		{"short phone", `{"phone":"123"}`, nil, http.StatusBadRequest},
		{"long text", fmt.Sprintf(`{"phone":"919886473508","text":%q}`, strings.Repeat("a", maxTextLen+1)), nil, http.StatusBadRequest},
		{"not on whatsapp", ok, fmt.Errorf("step 1: %w", errNotOnWhatsApp), http.StatusUnprocessableEntity},
		{"timeout", ok, context.DeadlineExceeded, http.StatusGatewayTimeout},
		{"upstream error", ok, errors.New("boom"), http.StatusBadGateway},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPhone string
			if rec := postFlow(flowHandler(fakeFlow(tc.flowErr, &gotPhone)), tc.body); rec.Code != tc.want {
				t.Errorf("status %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}
