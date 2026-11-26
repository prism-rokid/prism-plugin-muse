package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplyAfterKeepsReplyWithinSubmittedMessage(t *testing.T) {
	messages := []message{
		{ID: "other-user", Role: "user", Text: "another prompt"},
		{ID: "other-reply", Role: "assistant", Text: "another reply"},
		{ID: "submitted", Role: "user", Text: "test"},
		{ID: "early", Role: "assistant", Text: "working"},
		{ID: "final", Role: "assistant", Text: "done"},
		{ID: "next-user", Role: "user", Text: "next prompt"},
		{ID: "next-reply", Role: "assistant", Text: "next reply"},
	}
	if got := replyAfter(messages, "submitted"); got.ID != "final" {
		t.Fatalf("got reply %q, want final", got.ID)
	}
	if got := replyAfter(messages, "unknown"); got.ID != "" {
		t.Fatalf("unmatched user message got reply %q", got.ID)
	}
}

func TestMainChatIdentity(t *testing.T) {
	for _, path := range []string{"/", "/thread/main"} {
		if !isMainPath(path) {
			t.Fatalf("expected %q to identify the main chat", path)
		}
	}
	for _, path := range []string{"/thread/new", "/thread/old-side-chat", "/settings"} {
		if isMainPath(path) {
			t.Fatalf("unexpected main chat path %q", path)
		}
	}
	if _, err := conversationThread(mainThread, mainThread); err != nil {
		t.Fatal(err)
	}
	side := "47b11a3c-f1d7-4607-8744-cf09d866b4dc"
	if got := threadFromPath(chatPath(side)); got != side {
		t.Fatalf("side chat resolved to %q", got)
	}
	if _, err := conversationThread(mainThread, side); err == nil {
		t.Fatal("a mismatched side-chat binding must not route to the main chat")
	}
}

func TestHistoryKeepsPrismIdentityWhenMuseChangesUserID(t *testing.T) {
	prior := message{ID: "prior", Role: "user", Text: "repeated text"}
	match := newDeliveryMatch(prior.Text, []message{prior})
	active := delivery{Thread: mainThread, Message: "optimistic", Match: match}
	a := adapter{
		active: active,
		state:  savedState{Deliveries: map[string]delivery{"request": active}},
	}
	for _, nativeID := range []string{"optimistic", "persisted"} {
		p := page{Messages: []message{prior, {ID: nativeID, Role: "user", Text: prior.Text}}}
		turns := history(p, 5, a.historyIdentities(mainThread, p.Messages))
		if len(turns) != 2 || turns[0].TurnID != prior.ID || turns[1].TurnID != "user:request" ||
			turns[1].Messages[0].ID != "user:request" {
			t.Fatalf("unexpected turns for native ID %q: %#v", nativeID, turns)
		}
	}
}

func TestMuseProbeDiagnostics(t *testing.T) {
	for _, fixture := range []struct {
		page page
		err  error
		code string
	}{
		{err: os.ErrNotExist, code: "app_not_installed"},
		{err: errors.New("fixture token=/private/path"), code: "control_channel_unreachable"},
		{page: page{NeedsSignIn: true}, code: "authentication_required"},
		{page: page{Error: "fixture token"}, err: errors.New("fixture token"), code: "page_unavailable"},
		{code: "page_unavailable"},
		{page: page{Ready: true}, code: "ready"},
	} {
		result := museDiagnostic(fixture.page, fixture.err)
		if result.Code != fixture.code || strings.Contains(result.Detail, "fixture token") {
			t.Fatalf("意外的诊断结果 %#v", result)
		}
	}
}

func TestMuseProbeStdioMissingWorker(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(filepath.Dir(executable), "Muse Web.app", "Contents", "MacOS", "MuseWeb")
	if _, err := os.Stat(worker); !errors.Is(err, os.ErrNotExist) {
		t.Skip("该测试需要独立构建目录中没有 WebView worker")
	}
	var output bytes.Buffer
	request := strings.NewReader("{\"id\":\"probe\",\"method\":\"adapter.probe\"}\n")
	if err := serve(context.Background(), &adapter{}, request, &output); err != nil {
		t.Fatal(err)
	}
	var response struct {
		OK      bool
		Payload struct {
			UnavailableReason string `json:"unavailable_reason"`
			Diagnostics       struct{ Code string }
		}
	}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.Payload.Diagnostics.Code != "app_not_installed" || response.Payload.UnavailableReason == "" {
		t.Fatalf("意外的探测结果 %s", output.String())
	}
}
