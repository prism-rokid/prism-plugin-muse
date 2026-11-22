package main

import "testing"

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
