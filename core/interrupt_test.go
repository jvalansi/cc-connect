package core

import (
	"strings"
	"testing"
	"time"
)

// interruptibleSession ends the running turn with an empty result when
// interrupted, like Claude Code does.
type interruptibleSession struct {
	*queuingAgentSession
	interrupts chan struct{}
}

func (s *interruptibleSession) InterruptTurn() error {
	s.interrupts <- struct{}{}
	return nil
}

func runInterruptTurn(t *testing.T, enabled bool) (sent []string, interrupts int, replies []string) {
	t.Helper()
	p := &stubPlatformEngine{n: "test"}
	sess := &interruptibleSession{newQueuingSession("int"), make(chan struct{}, 100)}
	e := NewEngine("test", &controllableAgent{nextSession: sess}, []Platform{p}, "", LangEnglish)
	e.SetInterruptOnMessage(enabled)

	key := "test:user1"
	session := e.sessions.GetOrCreateActive(key)
	state := &interactiveState{agentSession: sess, platform: p, replyCtx: "ctx1"}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	sendDone := make(chan error, 1)
	sendDone <- nil
	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, key, "m1", time.Now(), nil, sendDone, "ctx1")
		close(done)
	}()

	queue := func(text string) {
		if !e.queueMessageForBusySession(p, &Message{SessionKey: key, Content: text, ReplyCtx: text}, key) {
			t.Fatal("message not queued")
		}
	}
	// "two" arrives before the turn has produced an event: no interrupt yet.
	queue("two")
	if n := len(sess.interrupts); n != 0 {
		t.Fatalf("interrupted before the turn started: %d", n)
	}
	sess.events <- Event{Type: EventToolUse, ToolName: "Bash"}
	if enabled {
		select {
		case <-sess.interrupts:
			interrupts++
		case <-time.After(2 * time.Second):
			t.Fatal("turn was not interrupted")
		}
		// A lost interrupt is resent even if the agent stays silent.
		select {
		case <-sess.interrupts:
			interrupts++
		case <-time.After(3 * time.Second):
			t.Fatal("interrupt was not retried")
		}
	}
	queue("three")
	if enabled {
		// Cancelled by the interrupt; prompting would block the loop forever.
		sess.events <- Event{Type: EventPermissionRequest, RequestID: "r", ToolName: "Bash"}
	}
	sess.events <- Event{Type: EventResult, Done: true}

	want := 2
	if enabled {
		want = 1 // both messages merge into one turn
	}
	for i := 1; i <= want; i++ {
		deadline := time.Now().Add(2 * time.Second)
		for {
			sess.sendMu.Lock()
			n := len(sess.sendCalls)
			sess.sendMu.Unlock()
			if n >= i {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("turn %d never sent", i)
			}
			time.Sleep(5 * time.Millisecond)
		}
		sess.events <- Event{Type: EventResult, Content: "answer", Done: true}
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("event loop did not finish")
	}
	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()
	return append(sent, sess.sendCalls...), interrupts + len(sess.interrupts), p.getSent()
}

func TestInterruptOnMessage(t *testing.T) {
	sent, interrupts, replies := runInterruptTurn(t, true)
	if interrupts == 0 || len(sent) != 1 || !strings.Contains(sent[0], "two") || !strings.Contains(sent[0], "three") {
		t.Fatalf("interrupts=%d sent=%q, want one merged turn", interrupts, sent)
	}
	if all := strings.Join(replies, "|"); strings.Contains(all, NewI18n(LangEnglish).T(MsgEmptyResponse)) || !strings.Contains(all, "answer") {
		t.Fatalf("replies=%q", replies)
	}
}

func TestInterruptOnMessage_DisabledQueues(t *testing.T) {
	sent, interrupts, _ := runInterruptTurn(t, false)
	if interrupts != 0 || len(sent) != 2 {
		t.Fatalf("interrupts=%d sent=%q, want two queued turns", interrupts, sent)
	}
}
