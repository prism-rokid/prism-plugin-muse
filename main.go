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
	"reflect"
	"strings"
	"sync"
	"time"

	sdk "github.com/Rokid-Prism/prism-plugin-sdk"
)

const (
	id         = "muse"
	surface    = "muse-web"
	mainThread = "main"
	mainTitle  = "主要聊天"
)

func isMainPath(path string) bool { return path == "/" || path == "/thread/main" }

type message struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	Text string `json:"text"`
}

type page struct {
	Ready           bool      `json:"ready"`
	OK              bool      `json:"ok"`
	Busy            bool      `json:"busy"`
	NeedsSignIn     bool      `json:"needs_sign_in"`
	NotSubmitted    bool      `json:"not_submitted"`
	WindowOpen      bool      `json:"window_open"`
	Path            string    `json:"path"`
	Error           string    `json:"error"`
	Messages        []message `json:"messages"`
	Chats           []chatRow `json:"chats"`
	CatalogComplete bool      `json:"catalog_complete"`
}

type browser struct {
	command *exec.Cmd
	input   io.WriteCloser
	output  *bufio.Scanner
}

func (b *browser) close() {
	if b.command != nil {
		_ = b.input.Close()
		_ = b.command.Process.Kill()
		_ = b.command.Wait()
		b.command = nil
	}
}

func (b *browser) request(ctx context.Context, operation, path, text string) (page, error) {
	if err := ctx.Err(); err != nil {
		return page{}, err
	}
	if b.command == nil {
		exe, err := os.Executable()
		if err != nil {
			return page{}, err
		}
		cmd := exec.Command(filepath.Join(filepath.Dir(exe), "Muse Web.app", "Contents", "MacOS", "MuseWeb"), "--plugin-worker")
		in, err := cmd.StdinPipe()
		if err != nil {
			return page{}, err
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			_ = in.Close()
			return page{}, err
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			_ = in.Close()
			_ = out.Close()
			return page{}, err
		}
		b.command, b.input, b.output = cmd, in, bufio.NewScanner(out)
		b.output.Buffer(make([]byte, 64*1024), 32*1024*1024)
	}
	type result struct {
		page page
		err  error
	}
	resultCh := make(chan result, 1)
	input, output := b.input, b.output
	go func() {
		if err := json.NewEncoder(input).Encode(map[string]string{"operation": operation, "path": path, "text": text}); err != nil {
			resultCh <- result{err: err}
			return
		}
		if !output.Scan() {
			resultCh <- result{err: errors.New("Muse Web window closed")}
			return
		}
		var response page
		err := json.Unmarshal(output.Bytes(), &response)
		resultCh <- result{page: response, err: err}
	}()
	timeout := time.NewTimer(12 * time.Second)
	defer timeout.Stop()
	select {
	case reply := <-resultCh:
		if reply.err != nil {
			b.close()
			return page{}, reply.err
		}
		if reply.page.Error != "" {
			return reply.page, errors.New(reply.page.Error)
		}
		return reply.page, nil
	case <-ctx.Done():
		// Panel cancels background reads when switching chats. Drain the
		// response so the next request can keep using the same WebView.
		drain := time.NewTimer(750 * time.Millisecond)
		defer drain.Stop()
		select {
		case reply := <-resultCh:
			if reply.err != nil {
				b.close()
			}
		case <-drain.C:
			b.close()
		case <-timeout.C:
			b.close()
		}
		return page{}, ctx.Err()
	case <-timeout.C:
		b.close()
		return page{}, context.DeadlineExceeded
	}
}

type delivery struct {
	Thread          string
	Message         string
	ResolvedMessage string         `json:",omitempty"`
	Match           *deliveryMatch `json:",omitempty"`
}

type savedState struct {
	Chats       map[string]chat `json:",omitempty"`
	MainUpdated time.Time
	Deliveries  map[string]delivery
}

type adapter struct {
	mu                sync.Mutex
	web               browser
	state             savedState
	statePath         string
	active            delivery
	activeSince       time.Time
	idleSince         time.Time
	waiting           int
	selectionRevision uint64
	catalogRevision   uint64
}

func loadAdapter() (*adapter, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	dir = filepath.Join(dir, "Prism Muse")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	a := &adapter{statePath: filepath.Join(dir, "sessions.json")}
	data, err := os.ReadFile(a.statePath)
	if err == nil {
		if err := json.Unmarshal(data, &a.state); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if a.state.Deliveries == nil {
		a.state.Deliveries = make(map[string]delivery)
	}
	if a.state.Chats == nil {
		a.state.Chats = make(map[string]chat)
	}
	return a, nil
}

func (a *adapter) save() error {
	data, err := json.Marshal(a.state)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(a.statePath), "sessions-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	closeErr := file.Close()
	if err := errors.Join(err, closeErr); err != nil {
		return err
	}
	return os.Rename(file.Name(), a.statePath)
}

func (a *adapter) ID() string { return id }

func (a *adapter) Probe(ctx context.Context) (sdk.Capability, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	starting := a.web.command == nil
	p, err := a.web.request(ctx, "snapshot", "", "")
	if starting {
		for attempt := 0; attempt < 10 && err == nil && !p.Ready; attempt++ {
			if delay(ctx, 350*time.Millisecond) != nil {
				break
			}
			p, err = a.web.request(ctx, "snapshot", "", "")
		}
	}
	c := sdk.Capability{
		PluginID: id, IntegrationMode: sdk.IntegrationModeDesktopAutomation, VisibilitySurface: surface,
		NativeVisibleInput: true, NativeVisibleOutput: true, CanForwardSync: true, CanListSessions: true,
		CanAttachSession: true, CanStartSessionWithMessage: true, CanReadHistory: true,
		CanWaitRun: true, CanReverseSync: true, CanPluginWideWatch: true, CanControlSession: true,
	}
	c.Available = err == nil && p.Ready
	d := museDiagnostic(p, err)
	c.Diagnostics = d
	if !c.Available {
		c.UnavailableReason = d.Summary
		if d.Detail != "" {
			c.UnavailableReason = d.Detail
		}
	}
	return c, nil
}

func museDiagnostic(p page, err error) *sdk.Diagnostic {
	facts := map[string]any{"transport": "webview", "transport_reachable": err == nil || p.Error != ""}
	d := &sdk.Diagnostic{Component: "Muse Web", Facts: facts, Actions: []string{"recheck"}}
	switch {
	case errors.Is(err, os.ErrNotExist):
		d.State, d.Code, d.Summary = "not_installed", "app_not_installed", "Muse Web 组件缺失"
		d.Detail = "请检查插件安装包是否包含 Muse Web。"
	case err != nil && p.Error == "":
		d.State, d.Code, d.Summary = "unavailable", "control_channel_unreachable", "Muse Web 未能连接"
		d.Detail = "读取 WebView 页面状态失败。"
	case err == nil && p.Ready:
		d.State, d.Code, d.Summary = "ready", "ready", "Muse 已连接"
		d.Actions = nil
	case p.NeedsSignIn:
		d.State, d.Code, d.Summary = "unavailable", "authentication_required", "Muse 尚未登录"
		d.Detail = "请在 Muse Web 中完成登录。"
	default:
		d.State, d.Code, d.Summary = "unavailable", "page_unavailable", "Muse Web 页面尚未就绪"
	}
	return d
}

func (a *adapter) Discover(ctx context.Context) (sdk.DiscoveryResult, error) {
	c, err := a.Probe(ctx)
	return sdk.DiscoveryResult{PluginID: id, Surface: surface, Verified: c.Available, Detail: c.UnavailableReason}, err
}

// Panel groups conversations by project path. Muse uses the plugin state directory.
func (a *adapter) project() string { return filepath.Dir(a.statePath) }

func (a *adapter) nativeSession(thread string) sdk.NativeSession {
	return sdk.NativeSession{PluginID: id, NativeSessionID: thread, NativeThreadID: thread, Surface: surface, Cwd: a.project(), Visible: true}
}

func (a *adapter) AttachSession(_ context.Context, req sdk.AttachSessionRequest) (sdk.NativeSession, error) {
	thread, err := conversationThread(req.NativeThreadID, req.NativeSessionID)
	if err != nil {
		return sdk.NativeSession{}, err
	}
	return a.nativeSession(thread), nil
}

func delay(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *adapter) selectThread(ctx context.Context, thread string) (page, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	navigated := false
	for {
		current, err := a.web.request(ctx, "snapshot", "", "")
		if err != nil {
			return page{}, err
		}
		if current.NeedsSignIn {
			return page{}, errors.New("Open Muse WebView and finish signing in")
		}
		a.reconcileUnwatchedRun(current)
		if current.Ready && threadFromPath(current.Path) == thread {
			return current, nil
		}
		if !navigated && current.Path != "" && threadFromPath(current.Path) != thread {
			if a.active.Thread != "" && a.active.Thread != thread {
				return page{}, errors.New("Muse is waiting for a reply in another conversation")
			}
			if _, err := a.web.request(ctx, "navigate", chatPath(thread), ""); err != nil {
				return page{}, err
			}
			navigated = true
		}
		if delay(ctx, 250*time.Millisecond) != nil {
			return page{}, errors.New("Muse conversation is unavailable. Open Muse WebView to check its loading or sign-in status.")
		}
	}
}

// Start resolves to the same stable main chat identity, rather than creating a side chat.
func (a *adapter) StartSessionWithMessage(ctx context.Context, req sdk.StartSessionWithMessageRequest) (sdk.StartSessionWithMessageResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d, err := a.submit(ctx, mainThread, req.Message)
	if err != nil {
		return sdk.StartSessionWithMessageResult{}, err
	}
	return sdk.StartSessionWithMessageResult{Session: a.nativeSession(d.Thread), Receipt: receipt(d),
		Visibility: sdk.VisibilityResult{Visible: true, Marker: req.Message.Text, Evidence: "Muse rendered the new user message in the main chat", CheckedAt: time.Now()}}, nil
}

func receipt(d delivery) sdk.SendReceipt {
	return sdk.SendReceipt{NativeMessageID: d.Message, Accepted: true, Visible: true,
		CanonicalNativeSessionID: d.Thread, CanonicalNativeThreadID: d.Thread}
}

func (a *adapter) Send(ctx context.Context, session sdk.NativeSession, msg sdk.InboundMessage) (sdk.SendReceipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	thread, err := conversationThread(session.NativeThreadID, session.NativeSessionID)
	if err != nil {
		return sdk.SendReceipt{}, err
	}
	d, err := a.submit(ctx, thread, msg)
	if err != nil {
		return sdk.SendReceipt{}, err
	}
	return receipt(d), nil
}

func normalize(text string) string { return strings.Join(strings.Fields(text), " ") }

func (a *adapter) submit(ctx context.Context, thread string, msg sdk.InboundMessage) (delivery, error) {
	if msg.PrismMessageID == "" || strings.TrimSpace(msg.Text) == "" || len(msg.Attachments) != 0 {
		return delivery{}, errors.New("Muse requires a message ID and text, without attachments")
	}
	if d, exists := a.state.Deliveries[msg.PrismMessageID]; exists {
		if d.Thread != thread {
			return delivery{}, errors.New("Muse message delivery is indeterminate or targets another conversation")
		}
		if d.Message != "" {
			return d, nil
		}
		if !d.Match.accepts(msg.Text) {
			return delivery{}, errors.New("Muse message delivery could not be reconciled")
		}
		if _, err := a.selectThread(ctx, thread); err != nil {
			return delivery{}, err
		}
		return a.confirmDelivery(ctx, msg.PrismMessageID, d)
	}
	if a.active.Thread != "" {
		p, err := a.web.request(ctx, "snapshot", "", "")
		if err != nil {
			return delivery{}, err
		}
		a.reconcileUnwatchedRun(p)
		if a.active.Thread != "" {
			return delivery{}, errors.New("Muse is waiting for the previous reply")
		}
	}
	p, err := a.selectThread(ctx, thread)
	if err != nil {
		return delivery{}, err
	}
	if p.Busy {
		return delivery{}, errors.New("Muse is running in the selected chat")
	}
	d := delivery{Thread: thread, Match: newDeliveryMatch(msg.Text, p.Messages)}
	a.state.Deliveries[msg.PrismMessageID] = d
	if err := a.save(); err != nil {
		delete(a.state.Deliveries, msg.PrismMessageID)
		return delivery{}, err
	}
	result, err := a.web.request(ctx, "submit", p.Path, msg.Text)
	if result.NotSubmitted {
		delete(a.state.Deliveries, msg.PrismMessageID)
		if saveErr := a.save(); saveErr != nil {
			a.state.Deliveries[msg.PrismMessageID] = d
			return delivery{}, errors.Join(err, saveErr)
		}
		if err == nil {
			err = errors.New("Muse did not submit the message")
		}
		return delivery{}, err
	}
	if err != nil {
		return delivery{}, fmt.Errorf("Muse submission requires confirmation: %w", err)
	}
	if !result.OK {
		return delivery{}, errors.New("Muse submission returned no confirmation")
	}
	return a.confirmDelivery(ctx, msg.PrismMessageID, d)
}

// Muse assigns independent turn IDs to user and assistant messages. Keep the
// reply within the transcript interval following the exact submitted message.
func replyAfter(messages []message, userID string) message {
	found := false
	var reply message
	for _, item := range messages {
		if item.Role == "user" {
			if found {
				break
			}
			found = item.ID == userID
		} else if found && item.Role == "assistant" && item.Text != "" {
			reply = item
		}
	}
	return reply
}

func (a *adapter) WaitForRun(ctx context.Context, session sdk.NativeSession, runID string) (sdk.PluginEvent, error) {
	thread, err := conversationThread(session.NativeThreadID, session.NativeSessionID)
	if err != nil {
		return sdk.PluginEvent{}, err
	}
	wait, cancel := context.WithTimeout(ctx, runWaitLimit)
	defer cancel()
	a.mu.Lock()
	d := a.deliveryForRun(thread, runID)
	tracked := a.active.Thread == thread && a.active.Message == runID
	if tracked {
		a.waiting++
	}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if tracked {
			a.waiting--
		}
		if a.waiting == 0 && a.active.Thread == thread && a.active.Message == runID {
			a.clearActive()
		}
		a.mu.Unlock()
	}()
	a.mu.Lock()
	_, err = a.selectThread(wait, thread)
	a.mu.Unlock()
	if err != nil {
		return sdk.PluginEvent{}, err
	}
	var previous message
	var since time.Time
	var lastRestore time.Time
	var idleWithoutReply time.Time
	for delay(wait, 400*time.Millisecond) == nil {
		a.mu.Lock()
		p, err := a.web.request(wait, "snapshot", "", "")
		replyTo := d.nativeMessage()
		if err == nil && p.Ready {
			current := threadFromPath(p.Path)
			if current == thread {
				replyTo = d.messageIn(p.Messages)
			} else if current != "" && !p.WindowOpen && time.Since(lastRestore) >= 3*time.Second {
				lastRestore = time.Now()
				_, _ = a.web.request(wait, "navigate", chatPath(thread), "")
			}
		}
		a.mu.Unlock()
		if p.NeedsSignIn {
			return sdk.PluginEvent{}, errors.New("Open Muse WebView and finish signing in")
		}
		if err != nil || !p.Ready || threadFromPath(p.Path) != thread {
			previous = message{}
			idleWithoutReply = time.Time{}
			continue
		}
		candidate := replyAfter(p.Messages, replyTo)
		if !p.Busy && strings.TrimSpace(candidate.Text) == "" {
			if idleWithoutReply.IsZero() {
				idleWithoutReply = time.Now()
			}
			if time.Since(idleWithoutReply) >= 25*time.Second {
				return sdk.PluginEvent{}, errors.New("Muse finished without a visible reply")
			}
		} else {
			idleWithoutReply = time.Time{}
		}
		if p.Busy || strings.TrimSpace(candidate.Text) == "" {
			previous = message{}
			continue
		}
		if candidate != previous {
			previous, since = candidate, time.Now()
			continue
		}
		if time.Since(since) >= 1500*time.Millisecond {
			a.mu.Lock()
			a.completeDelivery(thread, runID, replyTo)
			a.mu.Unlock()
			return sdk.PluginEvent{ID: "muse:" + candidate.ID, Type: "run.completed", Status: "completed",
				Summary: candidate.Text, CreatedAt: time.Now()}, nil
		}
	}
	return sdk.PluginEvent{}, errors.New("Muse reply completion could not be verified")
}

func (a *adapter) VerifyVisibility(ctx context.Context, session sdk.NativeSession, marker string) (sdk.VisibilityResult, error) {
	thread, err := conversationThread(session.NativeThreadID, session.NativeSessionID)
	if err != nil {
		return sdk.VisibilityResult{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	p, err := a.selectThread(ctx, thread)
	if err != nil {
		return sdk.VisibilityResult{}, err
	}
	for _, m := range p.Messages {
		if m.Role == "user" && normalize(m.Text) == normalize(marker) {
			return sdk.VisibilityResult{Visible: true, Marker: marker, Evidence: "Muse message " + m.ID, CheckedAt: time.Now()}, nil
		}
	}
	return sdk.VisibilityResult{Marker: marker, CheckedAt: time.Now()}, nil
}

func history(p page, limit int, identities map[string]string) []sdk.HistoryTurn {
	if limit <= 0 {
		limit = 5
	}
	limit = min(limit, 20)
	var turns []sdk.HistoryTurn
	for index, m := range p.Messages {
		publicID := m.ID
		if m.Role == "user" {
			if identity := identities[m.ID]; identity != "" {
				publicID = identity
			}
			turns = append(turns, sdk.HistoryTurn{TurnID: publicID, OrderKey: fmt.Sprintf("%08d", index), Revision: 1})
		}
		if len(turns) == 0 || m.Text == "" {
			continue
		}
		turn := &turns[len(turns)-1]
		turn.Messages = append(turn.Messages, sdk.HistoryMessage{ID: publicID, Role: m.Role, Type: "text", Content: m.Text, Status: "completed"})
	}
	if len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return turns
}

func (a *adapter) ReadHistoryStream(ctx context.Context, session sdk.NativeSession, req sdk.HistoryStreamRequest) (<-chan sdk.HistoryStreamEvent, error) {
	thread, err := conversationThread(session.NativeThreadID, session.NativeSessionID)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	p, err := a.selectThread(ctx, thread)
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	ch := make(chan sdk.HistoryStreamEvent)
	go func() {
		defer close(ch)
		send := func(event sdk.HistoryStreamEvent) bool {
			event.StreamID = req.StreamID
			select {
			case ch <- event:
				return true
			case <-ctx.Done():
				return false
			}
		}
		known := make(map[string]sdk.HistoryTurn)
		source := "initial"
		for {
			a.mu.Lock()
			identities := a.historyIdentities(thread, p.Messages)
			a.mu.Unlock()
			turns := history(p, req.Limit, identities)
			for i := len(turns) - 1; i >= 0; i-- {
				t := turns[i]
				operation := "append"
				if last, ok := known[t.TurnID]; ok {
					if reflect.DeepEqual(last.Messages, t.Messages) {
						continue
					}
					t.OrderKey, t.Revision, operation = last.OrderKey, last.Revision+1, "replace"
				}
				known[t.TurnID] = t
				if !send(sdk.HistoryStreamEvent{Type: "turn", Source: source, Operation: operation, Turn: &t}) {
					return
				}
			}
			if source == "initial" {
				if !req.Live {
					send(sdk.HistoryStreamEvent{Type: "end", Source: source, Operation: "append"})
					return
				}
				if !send(sdk.HistoryStreamEvent{Type: "page_end", Source: source, Operation: "append"}) {
					return
				}
				source = "live"
			}
			for {
				if delay(ctx, 700*time.Millisecond) != nil {
					return
				}
				a.mu.Lock()
				current, err := a.web.request(ctx, "snapshot", "", "")
				a.mu.Unlock()
				if err == nil && current.Ready && threadFromPath(current.Path) == thread {
					p = current
					break
				}
			}
		}
	}()
	return ch, nil
}

func (a *adapter) ControlSession(ctx context.Context, req sdk.ControlSessionRequest) (sdk.ControlSessionResult, error) {
	if req.Action != "conversation.select" {
		return sdk.ControlSessionResult{}, errors.New("Muse only supports selecting a conversation")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	thread, err := conversationThread(req.Session.NativeThreadID, req.Session.NativeSessionID)
	if err != nil {
		return sdk.ControlSessionResult{}, err
	}
	if _, err := a.selectThread(ctx, thread); err != nil {
		return sdk.ControlSessionResult{}, err
	}
	// An explicit Panel selection needs a fresh foreground event, even
	// when the main chat was already displayed and its run state is unchanged.
	a.selectionRevision++
	return sdk.ControlSessionResult{OK: true, Action: req.Action, ThreadID: thread}, nil
}

func (a *adapter) SubscribePlugin(ctx context.Context) (<-chan sdk.PluginEvent, error) {
	events := make(chan sdk.PluginEvent)
	go func() {
		defer close(events)
		var last string
		var lastCatalog uint64
		var nextCatalog time.Time
		for ctx.Err() == nil {
			a.mu.Lock()
			if !time.Now().Before(nextCatalog) {
				_, _ = a.refreshCatalog(ctx)
				nextCatalog = time.Now().Add(3 * time.Second)
			}
			p, err := a.web.request(ctx, "snapshot", "", "")
			a.reconcileUnwatchedRun(p)
			thread := threadFromPath(p.Path)
			title := a.chatTitle(thread)
			listed := thread == mainThread || a.state.Chats[thread].FoundInMuse
			status := "idle"
			if p.Busy || (thread != "" && a.active.Thread == thread) {
				status = "running"
			}
			revision := a.selectionRevision
			catalogRevision := a.catalogRevision
			a.mu.Unlock()

			now := time.Now()
			if catalogRevision != lastCatalog {
				select {
				case events <- sdk.PluginEvent{
					ID: fmt.Sprintf("muse-catalog-%d", now.UnixNano()), Type: "desktop.session.directory.reconciled", CreatedAt: now,
				}:
					lastCatalog = catalogRevision
				case <-ctx.Done():
					return
				}
			}
			var key string
			var event sdk.PluginEvent
			if err == nil && p.Ready && listed {
				key = fmt.Sprintf("%s:%s:%d", thread, status, revision)
				event = sdk.PluginEvent{
					ID: fmt.Sprintf("muse-desktop-%d", now.UnixNano()), Type: "desktop.state.changed",
					Status: status, CreatedAt: now,
					Payload: map[string]any{
						"desktop_live": true,
						"native_session": map[string]any{
							"plugin_id": id, "native_session_id": thread, "native_thread_id": thread,
							"surface": surface, "cwd": a.project(),
						},
						"detail_snapshot": map[string]any{
							"title": title, "cwd": a.project(), "desktop_foreground": true, "detail_stale": false,
							"updated_at": now.UnixMilli(), "run": map[string]any{"status": status},
						},
					},
				}
			} else if err != nil || !p.Ready || thread == "" {
				key = "unavailable"
				event = sdk.PluginEvent{
					ID: fmt.Sprintf("muse-desktop-%d", now.UnixNano()), Type: "watcher.foreground.unavailable",
					Status: "idle", CreatedAt: now,
					Payload: map[string]any{"watcher": "foreground", "code": "muse_conversation_not_selected"},
				}
			}
			if key != "" && key != last {
				select {
				case events <- event:
					last = key
				case <-ctx.Done():
					return
				}
			}
			if delay(ctx, time.Second) != nil {
				return
			}
		}
	}()
	return events, nil
}

func (a *adapter) Subscribe(ctx context.Context, _ sdk.NativeSession) (<-chan sdk.PluginEvent, error) {
	ch := make(chan sdk.PluginEvent)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

func (a *adapter) Interrupt(context.Context, sdk.NativeSession, string) error {
	return errors.New("Muse remote interruption is not enabled")
}

func (a *adapter) Close(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.web.close()
	return nil
}

func main() {
	a, err := loadAdapter()
	if err == nil {
		ctx := context.Background()
		err = serve(ctx, a, os.Stdin, os.Stdout)
		_ = a.Close(ctx)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
