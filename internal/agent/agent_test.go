package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type fakeAdapter struct {
	id       string
	started  int
	configs  []SessionConfig
	sessions []*fakeSession
}

func (a *fakeAdapter) ID() string { return a.id }

func (a *fakeAdapter) Status(context.Context) Status {
	return Status{
		ID:            a.id,
		Name:          a.id,
		Available:     true,
		Authenticated: true,
		Efforts:       []string{"low", "high"},
	}
}

func (a *fakeAdapter) Start(_ context.Context, config SessionConfig) (Session, error) {
	a.started++
	a.configs = append(a.configs, config)
	session := &fakeSession{}
	a.sessions = append(a.sessions, session)
	return session, nil
}

type fakeSession struct {
	prompts    []string
	images     [][]Image
	closed     bool
	interrupts int
}

func (s *fakeSession) Send(_ context.Context, turn Turn, emit func(Event) error) error {
	s.prompts = append(s.prompts, turn.Message)
	s.images = append(s.images, turn.Images)
	return emit(Event{Type: EventDelta, Text: "answer:" + turn.Message})
}

func (s *fakeSession) Interrupt(context.Context) error {
	s.interrupts++
	return nil
}

func TestManagerPassesImageOnlyTurn(t *testing.T) {
	adapter := &fakeAdapter{id: "test"}
	manager := NewManager("", "", "", adapter)
	defer manager.Close()

	image := Image{
		Name:      "pixel.png",
		MediaType: "image/png",
		Data:      "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
	}
	err := manager.Send(context.Background(), TurnRequest{
		Provider: "test",
		Images:   []Image{image},
	}, func(Event) error { return nil })
	if err == nil {
		t.Fatal("expected image capability error")
	}

	capable := &imageAdapter{fakeAdapter: fakeAdapter{id: "images"}}
	manager = NewManager("", "", "", capable)
	defer manager.Close()
	err = manager.Send(context.Background(), TurnRequest{
		Provider: "images",
		Images:   []Image{image},
	}, func(Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(capable.sessions) != 1 || len(capable.sessions[0].images) != 1 || len(capable.sessions[0].images[0]) != 1 {
		t.Fatalf("image turn was not passed to provider: %#v", capable.sessions)
	}
}

type imageAdapter struct {
	fakeAdapter
}

func (a *imageAdapter) Status(context.Context) Status {
	status := a.fakeAdapter.Status(context.Background())
	status.ImageInput = true
	return status
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
		Model:    "test-model",
		Effort:   "high",
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
	if config := adapter.configs[0]; config.Model != "test-model" || config.Effort != "high" {
		t.Fatalf("provider config = %#v", config)
	}
	if got := adapter.sessions[0].prompts; len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("unexpected prompts: %#v", got)
	}
}

func TestManagerRejectsUnsupportedEffort(t *testing.T) {
	adapter := &fakeAdapter{id: "test"}
	manager := NewManager("", "", "", adapter)
	defer manager.Close()

	err := manager.Send(context.Background(), TurnRequest{
		Provider: "test",
		Effort:   "ultra",
		Message:  "hello",
	}, func(Event) error { return nil })
	if err == nil {
		t.Fatal("expected unsupported effort error")
	}
	if adapter.started != 0 {
		t.Fatalf("provider started %d sessions, want 0", adapter.started)
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

type blockingAdapter struct {
	session *blockingSession
}

func (a *blockingAdapter) ID() string { return "blocking" }

func (a *blockingAdapter) Status(context.Context) Status {
	return Status{
		ID:            a.ID(),
		Name:          "Blocking",
		Available:     true,
		Authenticated: true,
		Interrupt:     true,
	}
}

func (a *blockingAdapter) Start(context.Context, SessionConfig) (Session, error) {
	return a.session, nil
}

type blockingSession struct {
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

func (s *blockingSession) Send(context.Context, Turn, func(Event) error) error {
	s.startOnce.Do(func() { close(s.started) })
	<-s.release
	return ErrInterrupted
}

func (s *blockingSession) Interrupt(context.Context) error {
	s.stopOnce.Do(func() { close(s.release) })
	return nil
}

func (s *blockingSession) Close() error { return nil }

func TestManagerInterruptsActiveTurnWithoutDroppingConversation(t *testing.T) {
	session := &blockingSession{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	manager := NewManager("", "", "", &blockingAdapter{session: session})
	defer manager.Close()

	var (
		conversationID string
		eventsMu       sync.Mutex
		events         []Event
	)
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- manager.Send(context.Background(), TurnRequest{
			Provider: "blocking",
			Message:  "wait",
		}, func(event Event) error {
			eventsMu.Lock()
			events = append(events, event)
			if event.Type == EventMeta {
				conversationID = event.ConversationID
			}
			eventsMu.Unlock()
			return nil
		})
	}()

	<-session.started
	eventsMu.Lock()
	id := conversationID
	eventsMu.Unlock()
	if id == "" {
		t.Fatal("manager did not emit a conversation id")
	}
	if err := manager.Interrupt(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := <-sendDone; err != nil {
		t.Fatal(err)
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	if len(events) != 2 || events[0].Type != EventMeta || events[1].Type != EventInterrupted {
		t.Fatalf("unexpected events: %#v", events)
	}
	if err := manager.Interrupt(context.Background(), id); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("idle interrupt error = %v, want %v", err, ErrNoActiveTurn)
	}
}
