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
	"time"
	"wabot/wa"

	"go.mau.fi/whatsmeow/types"
)

// fakeFlow stands in for contactFlow and records the phone it was called with.
func fakeFlow(err error, gotPhone *string) flowFunc {
	return func(_ context.Context, phone, _ string) (wa.FlowState, error) {
		*gotPhone = phone
		if err != nil {
			return wa.FlowState{}, err
		}
		return wa.FlowState{
			Who:       types.NewJID(phone, types.DefaultUserServer),
			Profile:   wa.Profile{Name: "Asha", NameSource: "push name", About: "busy", Devices: 2},
			Emoji:     wa.DefaultReaction,
			MessageID: "3EB0TEST",
			Reaction:  wa.Sent{ID: "3EB0ACK"},
			Presence:  wa.Presence{Status: wa.PresenceOffline, LastSeen: time.Date(2026, 11, 7, 14, 0, 0, 0, time.UTC)},
			Timings:   []wa.StepTiming{{Step: "whois", Took: 300 * time.Millisecond}, {Step: "react", Took: 900 * time.Millisecond}},
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
	if resp.MessageID != "3EB0TEST" || resp.About != "busy" || resp.JID != "919886473508@s.whatsapp.net" || resp.Name != "Asha" {
		t.Errorf("response = %+v", resp)
	}
	if resp.Status != "offline" || resp.LastSeen != "2026-11-07T14:00:00Z" {
		t.Errorf("presence = %q last seen %q", resp.Status, resp.LastSeen)
	}
	if !resp.Reaction.Sent || resp.Reaction.ID != "3EB0ACK" {
		t.Errorf("reaction = %+v, want sent with ack ID", resp.Reaction)
	}
	if len(resp.Timings) != 2 || resp.Timings[1].Step != "react" || resp.Timings[1].MS != 900 || resp.TotalMS != 1200 {
		t.Errorf("timings = %+v total %d", resp.Timings, resp.TotalMS)
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
		{"not on whatsapp", ok, fmt.Errorf("step 1: %w", wa.ErrNotOnWhatsApp), http.StatusUnprocessableEntity},
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
