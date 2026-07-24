package agent

import (
	"context"
	"testing"
)

type fakeAdapter struct {
	id       string
	started  int
	sessions []*fakeSession
}

func (a *fakeAdapter) ID() string { return a.id }

func (a *fakeAdapter) Status(context.Context) Status {
	return Status{ID: a.id, Name: a.id, Available: true, Authenticated: true}
}

func (a *fakeAdapter) Start(_ context.Context, _ SessionConfig) (Session, error) {
	a.started++
	session := &fakeSession{}
	a.sessions = append(a.sessions, session)
	return session, nil
}

type fakeSession struct {
	prompts []string
	closed  bool
}

func (s *fakeSession) Send(_ context.Context, prompt string, emit func(Event) error) error {
	s.prompts = append(s.prompts, prompt)
	return emit(Event{Type: EventDelta, Text: "answer:" + prompt})
}

func (s *fakeSession) Close() error {
	s.closed = true
	return nil
}

func TestManagerReusesEphemeralProviderSession(t *testing.T) {
	adapter := &fakeAdapter{id: "test"}
	manager := NewManager("root", "http://localhost/mcp", "token", adapter)
	defer manager.Close()

	var first []Event
	err := manager.Send(context.Background(), TurnRequest{
		Provider: "test",
		Message:  "one",
	}, func(event Event) error {
		first = append(first, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 || first[0].Type != EventMeta || first[1].Text != "answer:one" || first[2].Type != EventDone {
		t.Fatalf("unexpected first events: %#v", first)
	}
	conversationID := first[0].ConversationID

	var second []Event
	err = manager.Send(context.Background(), TurnRequest{
		ConversationID: conversationID,
		Provider:       "test",
		Message:        "two",
	}, func(event Event) error {
		second = append(second, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if adapter.started != 1 {
		t.Fatalf("provider started %d sessions, want 1", adapter.started)
	}
	if got := adapter.sessions[0].prompts; len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("unexpected prompts: %#v", got)
	}
}

func TestManagerRejectsProviderSwitch(t *testing.T) {
	first := &fakeAdapter{id: "first"}
	second := &fakeAdapter{id: "second"}
	manager := NewManager("", "", "", first, second)
	defer manager.Close()

	var conversationID string
	err := manager.Send(context.Background(), TurnRequest{
		Provider: "first",
		Message:  "hello",
	}, func(event Event) error {
		if event.Type == EventMeta {
			conversationID = event.ConversationID
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = manager.Send(context.Background(), TurnRequest{
		ConversationID: conversationID,
		Provider:       "second",
		Message:        "hello",
	}, func(Event) error { return nil })
	if err == nil {
		t.Fatal("expected provider switch error")
	}
}
