package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"time"
)

type deliveryMatch struct {
	Fingerprint string
	Before      []string
	Started     time.Time
}

func messageFingerprint(text string) string {
	digest := sha256.Sum256([]byte(normalize(text)))
	return hex.EncodeToString(digest[:])
}

func newDeliveryMatch(text string, messages []message) *deliveryMatch {
	match := &deliveryMatch{Fingerprint: messageFingerprint(text), Started: time.Now()}
	for _, item := range messages {
		if item.Role == "user" {
			match.Before = append(match.Before, item.ID)
		}
	}
	return match
}

func (m *deliveryMatch) recent() bool {
	return m != nil && time.Since(m.Started) < 2*time.Minute
}

func (m *deliveryMatch) accepts(text string) bool {
	return m.recent() && m.Fingerprint == messageFingerprint(text)
}

func (m *deliveryMatch) candidate(messages []message) string {
	if !m.recent() {
		return ""
	}
	var matched string
	for _, item := range messages {
		if item.Role != "user" || item.ID == "" || slices.Contains(m.Before, item.ID) ||
			messageFingerprint(item.Text) != m.Fingerprint {
			continue
		}
		if matched != "" && matched != item.ID {
			return ""
		}
		matched = item.ID
	}
	return matched
}

func (a *adapter) confirmDelivery(ctx context.Context, requestID string, d delivery) (delivery, error) {
	wait, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var candidate string
	var since time.Time
	for delay(wait, 300*time.Millisecond) == nil {
		p, err := a.web.request(wait, "snapshot", "", "")
		current := ""
		if err == nil && p.Ready && threadFromPath(p.Path) == d.Thread {
			current = d.Match.candidate(p.Messages)
		}
		if current != candidate {
			candidate, since = current, time.Now()
			continue
		}
		// Muse renders a temporary user ID before reconciling the send.
		if candidate == "" || time.Since(since) < 2*time.Second {
			continue
		}
		pending := d
		d.Message = candidate
		a.noteActivity(d.Thread, time.Now())
		a.state.Deliveries[requestID] = d
		if err := a.save(); err != nil {
			a.state.Deliveries[requestID] = pending
			return delivery{}, err
		}
		if a.active.Thread == "" {
			a.active, a.activeSince = d, time.Now()
		}
		return d, nil
	}
	return delivery{}, errors.New("Muse submission is indeterminate: the new user message was not verified")
}

// Use the same identity as Prism's message.user.accepted event. The native
// receipt still carries the Muse ID for matching the reply and run.
func (a *adapter) historyIdentities(thread string, messages []message) map[string]string {
	identities := make(map[string]string)
	for requestID, d := range a.state.Deliveries {
		if d.Thread != thread || d.Message == "" {
			continue
		}
		identity := "user:" + requestID
		identities[d.nativeMessage()] = identity
		if d.Match != nil && a.active.Match == d.Match {
			identities[d.messageIn(messages)] = identity
		}
	}
	return identities
}

func (d delivery) nativeMessage() string {
	if d.ResolvedMessage != "" {
		return d.ResolvedMessage
	}
	return d.Message
}

func (d delivery) messageIn(messages []message) string {
	nativeID := d.nativeMessage()
	for _, item := range messages {
		if item.Role == "user" && item.ID == nativeID {
			return nativeID
		}
	}
	if candidate := d.Match.candidate(messages); candidate != "" {
		return candidate
	}
	return nativeID
}

func (a *adapter) deliveryForRun(thread, runID string) delivery {
	if a.active.Thread == thread && a.active.Message == runID {
		return a.active
	}
	for _, d := range a.state.Deliveries {
		if d.Thread == thread && (d.Message == runID || d.ResolvedMessage == runID) {
			return d
		}
	}
	return delivery{Thread: thread, Message: runID}
}

func (a *adapter) completeDelivery(thread, runID, messageID string) {
	for requestID, d := range a.state.Deliveries {
		if d.Thread == thread && d.Message == runID && d.Match != nil {
			if messageID != d.Message {
				d.ResolvedMessage = messageID
			}
			d.Match = nil
			a.state.Deliveries[requestID] = d
			_ = a.save()
			return
		}
	}
}

const runWaitLimit = 14 * time.Minute

func (a *adapter) clearActive() {
	a.active = delivery{}
	a.idleSince = time.Time{}
}

// A conversation.start may deliver the first message without assigning a Hub
// waiter. Release the send gate when the native conversation settles.
func (a *adapter) reconcileUnwatchedRun(p page) {
	if a.active.Thread == "" || a.waiting > 0 {
		return
	}
	now := time.Now()
	if now.Sub(a.activeSince) >= runWaitLimit {
		a.clearActive()
		return
	}
	if !p.Ready || threadFromPath(p.Path) != a.active.Thread || p.Busy {
		a.idleSince = time.Time{}
		return
	}
	if a.idleSince.IsZero() {
		a.idleSince = now
	}
	nativeID := a.active.messageIn(p.Messages)
	idle := now.Sub(a.idleSince)
	if idle >= 2*time.Second && replyAfter(p.Messages, nativeID).ID != "" {
		a.completeDelivery(a.active.Thread, a.active.Message, nativeID)
		a.clearActive()
	} else if idle >= 15*time.Second {
		a.clearActive()
	}
}
