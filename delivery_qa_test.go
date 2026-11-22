package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/Rokid-Prism/prism-plugin-sdk"
)

type waitingAdapter struct {
	*adapter
	started chan struct{}
	finish  chan struct{}
}

func (a *waitingAdapter) WaitForRun(ctx context.Context, _ sdk.NativeSession, _ string) (sdk.PluginEvent, error) {
	close(a.started)
	select {
	case <-a.finish:
		return sdk.PluginEvent{Type: "run.completed"}, nil
	case <-ctx.Done():
		return sdk.PluginEvent{}, ctx.Err()
	}
}

func TestServeHandlesAttachWhileWaitingForRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a := &waitingAdapter{adapter: &adapter{}, started: make(chan struct{}), finish: make(chan struct{})}
	input, requests, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer requests.Close()
	responses, output := io.Pipe()
	defer responses.Close()
	defer output.Close()
	serverDone := make(chan error, 1)
	go func() { serverDone <- serve(ctx, a, input, output) }()
	var response struct {
		ID string `json:"id"`
		OK bool   `json:"ok"`
	}
	read := json.NewDecoder(responses)
	assertResponse := func(want string) {
		t.Helper()
		decoded := make(chan error, 1)
		go func() { decoded <- read.Decode(&response) }()
		select {
		case err := <-decoded:
			if err != nil || response.ID != want || !response.OK {
				t.Fatalf("expected successful %s response, got %+v, %v", want, response, err)
			}
		case <-ctx.Done():
			t.Fatalf("no %s response: %v", want, ctx.Err())
		}
	}
	if _, err := fmt.Fprintln(requests, `{"id":"wait","method":"adapter.waitForRun","params":{"session":{"native_thread_id":"main"},"run_id":"run"}}`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.started:
	case <-ctx.Done():
		t.Fatal("run waiter did not start")
	}
	if _, err := fmt.Fprintln(requests, `{"id":"attach","method":"adapter.attachSession","params":{"native_thread_id":"main"}}`); err != nil {
		t.Fatal(err)
	}
	assertResponse("attach")
	close(a.finish)
	assertResponse("wait")
	_ = requests.Close()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("server did not close")
	}
}

func TestSubmitRetriesAfterDefiniteRejection(t *testing.T) {
	a := deliveryFixture(t, "definite")
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	msg := sdk.InboundMessage{PrismMessageID: "request", Text: "test text"}
	session := a.nativeSession(mainThread)
	if _, err := a.Send(ctx, session, msg); err == nil {
		t.Fatal("draft should reject the first send")
	}
	if _, ok := a.state.Deliveries[msg.PrismMessageID]; ok {
		t.Fatal("a definite rejection left the request reserved")
	}
	receipt, err := a.Send(ctx, session, msg)
	if err != nil || !receipt.Accepted || receipt.NativeMessageID != "native-message" {
		t.Fatalf("expected the retry to submit and acknowledge: %+v, %v", receipt, err)
	}
}

func TestSubmitPreservesReservationAfterAmbiguousFailure(t *testing.T) {
	a := deliveryFixture(t, "ambiguous")
	msg := sdk.InboundMessage{PrismMessageID: "request", Text: "test text"}
	session := a.nativeSession(mainThread)
	if _, err := a.Send(context.Background(), session, msg); err == nil {
		t.Fatal("expected an ambiguous first send")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	if _, err := a.Send(ctx, session, msg); err == nil {
		t.Fatal("a retry cannot acknowledge an unconfirmed submission")
	}
	current, err := a.web.request(context.Background(), "snapshot", "", "")
	if err != nil || len(current.Messages) != 0 {
		t.Fatalf("an ambiguous send must not be resubmitted: %+v, %v", current.Messages, err)
	}
	if d := a.state.Deliveries[msg.PrismMessageID]; d.Thread != mainThread || d.Match == nil || d.Message != "" {
		t.Fatalf("expected a pending reservation, got %+v", d)
	}
}

func TestStartWithoutHubWaiterAllowsLaterSend(t *testing.T) {
	a := deliveryFixture(t, "completed")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started, err := a.StartSessionWithMessage(ctx, sdk.StartSessionWithMessageRequest{
		Message: sdk.InboundMessage{PrismMessageID: "first", Text: "first prompt"},
	})
	if err != nil || started.Session.NativeThreadID != mainThread {
		t.Fatalf("expected the first message in main chat: %+v, %v", started, err)
	}
	a.idleSince = time.Now().Add(-3 * time.Second)
	receipt, err := a.Send(ctx, started.Session, sdk.InboundMessage{PrismMessageID: "next", Text: "next prompt"})
	if err != nil || !receipt.Accepted || receipt.NativeMessageID == started.Receipt.NativeMessageID {
		t.Fatalf("expected another send after native completion without a Hub waiter: %+v, %v", receipt, err)
	}
}

func TestCanceledReadKeepsWebViewWorker(t *testing.T) {
	a := deliveryFixture(t, "slow")
	worker := a.web.command
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.web.input = cancelAfterWrite{WriteCloser: a.web.input, cancel: cancel}
	if _, err := a.web.request(ctx, "snapshot", "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	p, err := a.web.request(context.Background(), "snapshot", "", "")
	if err != nil || !p.Ready || a.web.command != worker {
		t.Fatalf("expected a fresh snapshot from the same helper: %+v, %v", p, err)
	}
}

type cancelAfterWrite struct {
	io.WriteCloser
	cancel context.CancelFunc
}

func (w cancelAfterWrite) Write(data []byte) (int, error) {
	n, err := w.WriteCloser.Write(data)
	w.cancel()
	return n, err
}

func deliveryFixture(t *testing.T, mode string) *adapter {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDeliveryWebViewFixture$")
	cmd.Env = append(os.Environ(), "PRISM_MUSE_TEST_WORKER="+mode)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	a := &adapter{
		statePath: filepath.Join(t.TempDir(), "sessions.json"),
		state:     savedState{Deliveries: make(map[string]delivery), Chats: make(map[string]chat)},
		web:       browser{command: cmd, input: input, output: bufio.NewScanner(output)},
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	return a
}

func TestDeliveryWebViewFixture(t *testing.T) {
	mode := os.Getenv("PRISM_MUSE_TEST_WORKER")
	if mode == "" {
		return
	}
	requests := json.NewDecoder(os.Stdin)
	responses := json.NewEncoder(os.Stdout)
	var messages []message
	attempts := 0
	for {
		var req struct {
			Operation string `json:"operation"`
			Text      string `json:"text"`
		}
		if err := requests.Decode(&req); err != nil {
			break
		}
		result := page{Ready: true, Path: "/", Messages: messages}
		if mode == "slow" {
			time.Sleep(40 * time.Millisecond)
		}
		if req.Operation == "submit" {
			attempts++
			switch {
			case mode == "completed":
				messages = append(messages,
					message{ID: fmt.Sprintf("user-%d", attempts), Role: "user", Text: req.Text},
					message{ID: fmt.Sprintf("reply-%d", attempts), Role: "assistant", Text: "reply"},
				)
				result = page{OK: true}
			case attempts == 1 && mode == "definite":
				result = page{NotSubmitted: true, Error: "Muse already has an unsent draft"}
			case attempts == 1:
				result = page{Error: "Muse page was unavailable during the operation"}
			default:
				messages = append(messages, message{ID: "native-message", Role: "user", Text: req.Text})
				result = page{OK: true}
			}
		}
		if err := responses.Encode(result); err != nil {
			os.Exit(1)
		}
	}
	os.Exit(0)
}
