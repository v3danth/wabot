package wa

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

func TestPhoneToJID(t *testing.T) {
	want := types.NewJID("919886473508", types.DefaultUserServer)
	if got := PhoneToJID("+91 98864-73508"); got != want {
		t.Errorf("PhoneToJID = %s, want %s", got, want)
	}
}

func TestToJID(t *testing.T) {
	got, err := ToJID("278833789944039@lid")
	if err != nil || got.Server != types.HiddenUserServer || got.User != "278833789944039" {
		t.Errorf("ToJID(lid) = %v, %v", got, err)
	}
}

func TestRunStepsStopsAtFirstFailure(t *testing.T) {
	var ran []string
	step := func(name string, fail bool) FlowStep {
		return FlowStep{name, func(_ context.Context, _ *whatsmeow.Client, st FlowState) (FlowState, error) {
			ran = append(ran, name)
			if fail {
				return st, errors.New("boom")
			}
			st.Emoji += name
			return st, nil
		}}
	}
	st, err := RunSteps(context.Background(), nil, FlowState{},
		[]FlowStep{step("a", false), step("b", true), step("c", false)})
	if err == nil || !strings.Contains(err.Error(), `step 2/3 "b"`) {
		t.Errorf("err = %v, want failure naming step 2/3 b", err)
	}
	if !slices.Equal(ran, []string{"a", "b"}) {
		t.Errorf("ran %v, want [a b]", ran)
	}
	if st.Emoji != "a" {
		t.Errorf("state = %q, want result of last good step", st.Emoji)
	}
}

func TestContactName(t *testing.T) {
	tests := []struct {
		c          types.ContactInfo
		name, from string
	}{
		{types.ContactInfo{FullName: "Asha K", PushName: "asha"}, "Asha K", "contact"},
		{types.ContactInfo{PushName: "asha", BusinessName: "Shop"}, "asha", "push name"},
		{types.ContactInfo{BusinessName: "Shop"}, "Shop", "business"},
		{types.ContactInfo{}, "", ""},
	}
	for _, tc := range tests {
		if name, from := contactName(tc.c); name != tc.name || from != tc.from {
			t.Errorf("contactName(%+v) = %q, %q; want %q, %q", tc.c, name, from, tc.name, tc.from)
		}
	}
}

func TestSaveProfilePhoto(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("jpeg bytes"))
	}))
	defer srv.Close()
	dir := t.TempDir()
	path, err := SaveProfilePhoto(context.Background(), srv.URL, dir, "918317402174")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "profile_918317402174.jpg"); path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
	if got, _ := os.ReadFile(path); string(got) != "jpeg bytes" {
		t.Errorf("file = %q", got)
	}
}

func TestSavePhotoSkipsHiddenPicture(t *testing.T) {
	st, err := savePhoto(context.Background(), nil, FlowState{Phone: "91", PhotoDir: t.TempDir()})
	if err != nil || st.PhotoPath != "" {
		t.Errorf("savePhoto with no picture = %q, %v; want no file, no error", st.PhotoPath, err)
	}
}
