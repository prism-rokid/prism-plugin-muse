package main

import (
	"context"
	"errors"
	"maps"
	"regexp"
	"sort"
	"strings"
	"time"

	sdk "github.com/Rokid-Prism/prism-plugin-sdk"
)

var sideChatID = regexp.MustCompile(`(?i)^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$`)

type chat struct {
	Thread      string
	Title       string
	Updated     time.Time
	FoundInMuse bool `json:",omitempty"`
}

type chatRow struct {
	Thread  string `json:"thread"`
	Title   string `json:"title"`
	Updated int64  `json:"updated"`
}

func threadFromPath(path string) string {
	if isMainPath(path) {
		return mainThread
	}
	if thread, ok := strings.CutPrefix(path, "/thread/"); ok && sideChatID.MatchString(thread) {
		return thread
	}
	return ""
}

func chatPath(thread string) string {
	if thread == mainThread {
		return "/"
	}
	return "/thread/" + thread
}

func conversationThread(thread, session string) (string, error) {
	if thread == "" {
		thread = session
	}
	if (thread != mainThread && !sideChatID.MatchString(thread)) || (session != "" && session != thread) {
		return "", errors.New("Invalid Muse conversation binding")
	}
	return thread, nil
}

func (a *adapter) chatTitle(thread string) string {
	if thread == mainThread {
		return mainTitle
	}
	if title := a.state.Chats[thread].Title; title != "" {
		return title
	}
	return "旁聊"
}

func (a *adapter) noteActivity(thread string, at time.Time) {
	if thread == mainThread {
		a.state.MainUpdated = at
		return
	}
	item := a.state.Chats[thread]
	if item.FoundInMuse {
		item.Updated = at
		a.state.Chats[thread] = item
	}
}

func (a *adapter) refreshCatalog(ctx context.Context) (string, error) {
	catalog, err := a.web.request(ctx, "catalog", "", "")
	if err != nil {
		return "", nil
	}
	complete := catalog.CatalogComplete && catalog.Chats != nil
	rows := make(map[string]chatRow, len(catalog.Chats))
	for _, row := range catalog.Chats {
		if _, seen := rows[row.Thread]; !sideChatID.MatchString(row.Thread) || row.Title == "" || seen {
			complete = false
			continue
		}
		rows[row.Thread] = row
	}
	previousChats := a.state.Chats
	chats := maps.Clone(previousChats)
	changed := false
	for thread, row := range rows {
		previous := chats[thread]
		if !complete && !previous.FoundInMuse {
			continue
		}
		item := chat{Thread: thread, Title: row.Title, Updated: previous.Updated, FoundInMuse: true}
		if row.Updated > 0 && time.UnixMilli(row.Updated).After(item.Updated) {
			item.Updated = time.UnixMilli(row.Updated)
		}
		if item != previous {
			chats[thread] = item
			changed = true
		}
	}
	if complete {
		for thread := range chats {
			if _, seen := rows[thread]; !seen {
				delete(chats, thread)
				changed = true
			}
		}
	}
	if changed {
		a.state.Chats = chats
		if err := a.save(); err != nil {
			a.state.Chats = previousChats
			return "", err
		}
		a.catalogRevision++
	}
	return threadFromPath(catalog.Path), nil
}

func (a *adapter) ListSessions(ctx context.Context) ([]sdk.NativeSessionHint, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	active, err := a.refreshCatalog(ctx)
	if err != nil {
		return nil, err
	}
	if active != mainThread && !a.state.Chats[active].FoundInMuse {
		active = mainThread
	}
	hints := []sdk.NativeSessionHint{a.sessionHint(mainThread, active, a.state.MainUpdated)}
	for thread, item := range a.state.Chats {
		if item.FoundInMuse && sideChatID.MatchString(thread) {
			hints = append(hints, a.sessionHint(thread, active, item.Updated))
		}
	}
	sort.Slice(hints[1:], func(i, j int) bool {
		left, right := hints[i+1], hints[j+1]
		if left.LastActivityAt.Equal(right.LastActivityAt) {
			return left.NativeThreadID < right.NativeThreadID
		}
		return left.LastActivityAt.After(right.LastActivityAt)
	})
	return hints, nil
}

func (a *adapter) sessionHint(thread, active string, updated time.Time) sdk.NativeSessionHint {
	return sdk.NativeSessionHint{
		PluginID: id, NativeSessionID: thread, NativeThreadID: thread,
		Surface: surface, Title: a.chatTitle(thread), Cwd: a.project(), Active: thread == active,
		Visible: true, LastActivityAt: updated,
	}
}
