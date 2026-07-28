package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/access"
	"github.com/spolnik/RepoKarta/internal/contextscope"
)

func TestPromptWithContextsUsesResolvedIdentities(t *testing.T) {
	prompt := PromptWithContexts("Where is startup wired?", []contextscope.Context{{
		Kind:         contextscope.KindFile,
		RepositoryID: 7,
		Repository:   "RepoKarta",
		Revision:     strings.Repeat("a", 40),
		Path:         "internal/app/app.go",
		Label:        "@RepoKarta:internal/app/app.go",
	}})
	for _, expected := range []string{
		"repository_id=7",
		"revision=" + strings.Repeat("a", 40),
		`path="internal/app/app.go"`,
		"User question:\nWhere is startup wired?",
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("prompt does not contain %q: %s", expected, prompt)
		}
	}
	if strings.Contains(prompt, "@RepoKarta") {
		t.Fatalf("prompt parsed or reused display label: %s", prompt)
	}
}

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
		Models:        []ModelOption{{ID: "test-model", Label: "Test Model"}},
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
	turns      []Turn
	deadlines  []time.Duration
	closed     bool
	interrupts int
}

func (s *fakeSession) Send(ctx context.Context, turn Turn, emit func(Event) error) error {
	s.prompts = append(s.prompts, turn.Message)
	s.images = append(s.images, turn.Images)
	s.turns = append(s.turns, turn)
	if deadline, ok := ctx.Deadline(); ok {
		s.deadlines = append(s.deadlines, time.Until(deadline))
	}
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
	if len(first) != 4 ||
		first[0].Type != EventMeta ||
		first[1].Type != EventActivity ||
		first[1].Activity != ActivityThinking ||
		first[2].Text != "answer:one" ||
		first[3].Type != EventDone {
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

func TestManagerAppliesTurnTimeoutAndTokenBudget(t *testing.T) {
	adapter := &fakeAdapter{id: "test"}
	manager := NewManager("", "", "", adapter)
	defer manager.Close()

	if err := manager.Send(context.Background(), TurnRequest{
		Provider:       "test",
		Message:        "bounded",
		TimeoutSeconds: 300,
		TokenBudget:    2222,
	}, func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	session := adapter.sessions[0]
	if len(session.turns) != 1 || session.turns[0].TokenBudget != 2222 {
		t.Fatalf("turn budget = %#v", session.turns)
	}
	if len(session.deadlines) != 1 || session.deadlines[0] < 299*time.Second ||
		session.deadlines[0] > 300*time.Second {
		t.Fatalf("turn deadline = %v", session.deadlines)
	}
	for _, request := range []TurnRequest{
		{Provider: "test", Message: "bad timeout", TimeoutSeconds: MaximumTurnTimeoutSeconds + 1},
		{Provider: "test", Message: "bad budget", TokenBudget: MaximumTokenBudget + 1},
	} {
		if err := manager.Send(context.Background(), request, func(Event) error { return nil }); err == nil {
			t.Fatalf("invalid controls unexpectedly succeeded: %#v", request)
		}
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

type modelEffortAdapter struct {
	fakeAdapter
}

func (a *modelEffortAdapter) Status(context.Context) Status {
	return Status{
		ID:            a.id,
		Name:          a.id,
		Available:     true,
		Authenticated: true,
		Models: []ModelOption{
			{ID: "reasoning", Label: "Reasoning", Efforts: []string{"low", "medium"}},
			{ID: "fast", Label: "Fast", Efforts: []string{}},
		},
		Efforts: []string{"low", "medium"},
	}
}

func TestManagerHonorsModelSpecificEffortCapabilities(t *testing.T) {
	adapter := &modelEffortAdapter{fakeAdapter: fakeAdapter{id: "model-effort"}}
	manager := NewManager("", "", "", adapter)
	defer manager.Close()

	err := manager.Send(context.Background(), TurnRequest{
		Provider: "model-effort",
		Model:    "fast",
		Effort:   "medium",
		Message:  "hello",
	}, func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "does not support effort") {
		t.Fatalf("model-specific effort validation error = %v", err)
	}
	if adapter.started != 0 {
		t.Fatalf("provider started %d sessions, want 0", adapter.started)
	}
}

func TestManagerRejectsModelOutsideHarnessCatalog(t *testing.T) {
	adapter := &fakeAdapter{id: "test"}
	manager := NewManager("", "", "", adapter)
	defer manager.Close()

	err := manager.Send(context.Background(), TurnRequest{
		Provider: "test",
		Model:    "invented-model",
		Message:  "hello",
	}, func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "does not support model") {
		t.Fatalf("model validation error = %v", err)
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

type changingAuthAdapter struct {
	statuses    []bool
	statusCalls int
	startCalls  int
	startError  error
	sendError   error
}

func (a *changingAuthAdapter) ID() string { return "changing-auth" }

func (a *changingAuthAdapter) Status(context.Context) Status {
	index := min(a.statusCalls, len(a.statuses)-1)
	a.statusCalls++
	authenticated := len(a.statuses) > 0 && a.statuses[index]
	return Status{
		ID:            a.ID(),
		Name:          "Changing Auth",
		Available:     true,
		Authenticated: authenticated,
		Detail:        "authentication follows the process that launched RepoKarta",
	}
}

func (a *changingAuthAdapter) Start(context.Context, SessionConfig) (Session, error) {
	a.startCalls++
	if a.startError != nil {
		return nil, a.startError
	}
	return &failingAuthSession{sendError: a.sendError}, nil
}

type failingAuthSession struct {
	sendError error
}

func (s *failingAuthSession) Send(context.Context, Turn, func(Event) error) error {
	return s.sendError
}

func (*failingAuthSession) Interrupt(context.Context) error { return nil }
func (*failingAuthSession) Close() error                    { return nil }

func TestManagerRechecksCachedProviderReadinessForNewConversation(t *testing.T) {
	adapter := &changingAuthAdapter{statuses: []bool{true, false}}
	manager := NewManager("", "", "", adapter)
	defer manager.Close()

	statuses := manager.Statuses(context.Background())
	if len(statuses) != 1 || !statuses[0].Authenticated {
		t.Fatalf("initial statuses = %#v", statuses)
	}
	err := manager.Send(context.Background(), TurnRequest{
		Provider: adapter.ID(),
		Message:  "hello",
	}, func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "launch context") {
		t.Fatalf("send error = %v, want launch-context authentication error", err)
	}
	if adapter.statusCalls != 2 || adapter.startCalls != 0 {
		t.Fatalf("status calls = %d, start calls = %d", adapter.statusCalls, adapter.startCalls)
	}
}

func TestManagerRechecksAuthenticationAfterProviderStartupFailure(t *testing.T) {
	adapter := &changingAuthAdapter{
		statuses:   []bool{true, false},
		startError: errors.New("provider stream ended"),
	}
	manager := NewManager("", "", "", adapter)
	defer manager.Close()

	err := manager.Send(context.Background(), TurnRequest{
		Provider: adapter.ID(),
		Message:  "hello",
	}, func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "not authenticated in RepoKarta's launch context") {
		t.Fatalf("send error = %v, want refreshed launch-context authentication error", err)
	}
	if adapter.statusCalls != 2 || adapter.startCalls != 1 {
		t.Fatalf("status calls = %d, start calls = %d", adapter.statusCalls, adapter.startCalls)
	}
}

func TestManagerRechecksAuthenticationAfterProviderSessionFailure(t *testing.T) {
	adapter := &changingAuthAdapter{
		statuses:  []bool{true, false},
		sendError: errors.New("provider stream ended"),
	}
	manager := NewManager("", "", "", adapter)
	defer manager.Close()

	err := manager.Send(context.Background(), TurnRequest{
		Provider: adapter.ID(),
		Message:  "hello",
	}, func(Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "not authenticated in RepoKarta's launch context") {
		t.Fatalf("send error = %v, want refreshed launch-context authentication error", err)
	}
	if adapter.statusCalls != 2 || adapter.startCalls != 1 {
		t.Fatalf("status calls = %d, start calls = %d", adapter.statusCalls, adapter.startCalls)
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
	if len(events) != 3 ||
		events[0].Type != EventMeta ||
		events[1].Type != EventActivity ||
		events[2].Type != EventInterrupted {
		t.Fatalf("unexpected events: %#v", events)
	}
	if err := manager.Interrupt(context.Background(), id); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("idle interrupt error = %v, want %v", err, ErrNoActiveTurn)
	}
}

type memoryConversationStore struct {
	conversations map[string]Conversation
}

type segmentedAdapter struct{}

func (*segmentedAdapter) ID() string { return "segmented" }

func (a *segmentedAdapter) Status(context.Context) Status {
	return Status{ID: a.ID(), Name: "Segmented", Available: true, Authenticated: true}
}

func (*segmentedAdapter) Start(context.Context, SessionConfig) (Session, error) {
	return &segmentedSession{}, nil
}

type segmentedSession struct{}

func (*segmentedSession) Send(_ context.Context, _ Turn, emit func(Event) error) error {
	for _, event := range []Event{
		{Type: EventDelta, SegmentID: "message-1", Text: "First update."},
		{Type: EventActivity, Activity: ActivityThinking, SegmentID: "message-1"},
		{Type: EventDelta, SegmentID: "message-2", Text: "Final answer."},
	} {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}

func (*segmentedSession) Interrupt(context.Context) error { return nil }
func (*segmentedSession) Close() error                    { return nil }

func TestManagerPreservesProviderMessageBoundariesInTranscript(t *testing.T) {
	store := &memoryConversationStore{}
	manager := NewManager("", "", "", &segmentedAdapter{}).UsePersistence(store)
	defer manager.Close()

	var events []Event
	if err := manager.Send(context.Background(), TurnRequest{
		Provider: "segmented",
		Message:  "Show progress",
	}, func(event Event) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 6 ||
		events[1].Type != EventActivity ||
		events[2].SegmentID != "message-1" ||
		events[3].Type != EventActivity ||
		events[4].SegmentID != "message-2" {
		t.Fatalf("segmented events = %#v", events)
	}
	conversation, err := store.GetConversation(context.Background(), events[0].ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversation.Messages) != 2 {
		t.Fatalf("persisted messages = %#v", conversation.Messages)
	}
	if got := conversation.Messages[1].Text; got != "First update.\n\nFinal answer." {
		t.Fatalf("assistant transcript = %q", got)
	}
}

func TestManagerRunEphemeralReturnsOnlyFinalProviderMessage(t *testing.T) {
	store := &memoryConversationStore{}
	manager := NewManager("", "", "", &segmentedAdapter{}).UsePersistence(store)
	defer manager.Close()

	ctx := access.WithViewer(context.Background(), access.Viewer{
		ID: "saml:alice", Groups: []string{"engineering"},
	})
	ephemeralID := ""
	result, err := manager.RunEphemeral(ctx, TurnRequest{
		Provider: "segmented",
		Message:  "Generate repository documentation",
	}, func(event Event) error {
		if event.Type == EventMeta {
			ephemeralID = event.ConversationID
			author, ok := manager.AuthorForMCP(ephemeralID)
			if !ok || author.ID != "saml:alice" ||
				len(author.Groups) != 1 || author.Groups[0] != "engineering" {
				t.Fatalf("ephemeral MCP author = %#v, present = %t", author, ok)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != "segmented" || result.Text != "Final answer." {
		t.Fatalf("ephemeral result = %#v", result)
	}
	if len(store.conversations) != 0 {
		t.Fatalf("ephemeral generation created durable conversations: %#v", store.conversations)
	}
	if _, ok := manager.AuthorForMCP(ephemeralID); ok {
		t.Fatal("ephemeral MCP author remained after the provider turn")
	}
}

func (s *memoryConversationStore) CreateConversation(_ context.Context, conversation Conversation) error {
	if s.conversations == nil {
		s.conversations = make(map[string]Conversation)
	}
	s.conversations[conversation.ID] = conversation
	return nil
}

type emptyCitationSource struct{}

func (emptyCitationSource) List(string) []Citation { return nil }
func (emptyCitationSource) Clear(string)           {}

func TestDeepSearchPersistsModeAndVisibleTrace(t *testing.T) {
	store := &memoryConversationStore{}
	adapter := &fakeAdapter{id: "test"}
	manager := NewManager("", "", "", adapter).
		UsePersistence(store).
		UseCitations(emptyCitationSource{})
	defer manager.Close()

	var events []Event
	err := manager.Send(context.Background(), TurnRequest{
		Provider: "test",
		Message:  "Trace the request",
		Mode:     "deep_search",
	}, func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.conversations) != 1 {
		t.Fatalf("conversations = %#v", store.conversations)
	}
	var conversation Conversation
	for _, candidate := range store.conversations {
		conversation = candidate
	}
	if conversation.Mode != "deep_search" || len(conversation.Messages) != 2 {
		t.Fatalf("deep search conversation = %#v", conversation)
	}
	assistant := conversation.Messages[1]
	if len(assistant.Trace) != 4 ||
		assistant.Trace[0].Stage != "scope_resolved" ||
		assistant.Trace[3].Stage != "complete" ||
		assistant.Trace[2].CoverageWarning == "" {
		t.Fatalf("persisted trace = %#v", assistant.Trace)
	}
	if len(adapter.sessions) != 1 ||
		!strings.Contains(adapter.sessions[0].prompts[0], "Deep Search mode is active") {
		t.Fatalf("provider prompt = %#v", adapter.sessions)
	}
	traceEvents := 0
	for _, event := range events {
		if event.Type == EventTrace {
			traceEvents++
		}
	}
	if traceEvents != 4 {
		t.Fatalf("trace events = %d, want 4: %#v", traceEvents, events)
	}
}

func (s *memoryConversationStore) ListConversations(_ context.Context, filter ConversationFilter) ([]Conversation, error) {
	result := make([]Conversation, 0, len(s.conversations))
	for _, conversation := range s.conversations {
		if conversation.Author.ID != filter.AuthorID {
			continue
		}
		result = append(result, conversation)
	}
	return result, nil
}

func TestManagerEnforcesConversationAuthorWhenContinuing(t *testing.T) {
	store := &memoryConversationStore{conversations: map[string]Conversation{
		"saved": {
			ID:       "saved",
			Title:    "Alice's conversation",
			Provider: "test",
			Author: ConversationAuthor{
				ID:       "saml:alice",
				Name:     "Alice",
				Provider: "saml",
			},
		},
	}}
	adapter := &fakeAdapter{id: "test"}
	manager := NewManager("", "", "", adapter).UsePersistence(store)
	defer manager.Close()

	err := manager.Send(context.Background(), TurnRequest{
		ConversationID: "saved",
		Message:        "Continue",
		Author: ConversationAuthor{
			ID:       "saml:bob",
			Name:     "Bob",
			Provider: "saml",
		},
	}, func(Event) error { return nil })
	if !errors.Is(err, ErrConversationForbidden) {
		t.Fatalf("Send() error = %v, want ErrConversationForbidden", err)
	}
	if adapter.started != 0 {
		t.Fatalf("provider started %d times for an unauthorized author", adapter.started)
	}

	err = manager.Send(context.Background(), TurnRequest{
		ConversationID: "saved",
		Message:        "Continue with current groups",
		Author: ConversationAuthor{
			ID:       "saml:alice",
			Name:     "Alice",
			Provider: "saml",
			Groups:   []string{"current-team"},
		},
	}, func(Event) error { return nil })
	if err != nil {
		t.Fatalf("owner continuation failed: %v", err)
	}
	if got := store.conversations["saved"].Author.Groups; len(got) != 1 || got[0] != "current-team" {
		t.Fatalf("refreshed author groups = %#v", got)
	}

	err = manager.Send(context.Background(), TurnRequest{
		ConversationID: "saved",
		Message:        "Continue as administrator",
		Author:         ConversationAuthor{ID: "local:admin", Provider: "local"},
	}, func(Event) error { return nil })
	if !errors.Is(err, ErrConversationForbidden) {
		t.Fatalf("administrator continuation error = %v, want ErrConversationForbidden", err)
	}
}

func (s *memoryConversationStore) GetConversation(_ context.Context, id string) (Conversation, error) {
	conversation, ok := s.conversations[id]
	if !ok {
		return Conversation{}, ErrConversationNotFound
	}
	return conversation, nil
}

func (s *memoryConversationStore) AppendMessage(_ context.Context, message Message) (Message, error) {
	conversation, ok := s.conversations[message.ConversationID]
	if !ok {
		return Message{}, ErrConversationNotFound
	}
	message.ID = int64(len(conversation.Messages) + 1)
	conversation.Messages = append(conversation.Messages, message)
	conversation.MessageCount = len(conversation.Messages)
	conversation.InputTokens += message.InputTokens
	conversation.OutputTokens += message.OutputTokens
	s.conversations[conversation.ID] = conversation
	return message, nil
}

func (s *memoryConversationStore) RenameConversation(_ context.Context, id, title string) error {
	conversation, ok := s.conversations[id]
	if !ok {
		return ErrConversationNotFound
	}
	conversation.Title = title
	s.conversations[id] = conversation
	return nil
}

func (s *memoryConversationStore) UpdateConversationCursor(_ context.Context, id, cursor string) error {
	conversation, ok := s.conversations[id]
	if !ok {
		return ErrConversationNotFound
	}
	conversation.ResumeCursor = cursor
	s.conversations[id] = conversation
	return nil
}

func (s *memoryConversationStore) UpdateConversationAuthor(_ context.Context, id string, author ConversationAuthor) error {
	conversation, ok := s.conversations[id]
	current := normalizeConversationAuthor(conversation.Author)
	if !ok || current.ID != author.ID {
		return ErrConversationNotFound
	}
	conversation.Author = author
	s.conversations[id] = conversation
	return nil
}

func (s *memoryConversationStore) DeleteConversation(_ context.Context, id string) error {
	if _, ok := s.conversations[id]; !ok {
		return ErrConversationNotFound
	}
	delete(s.conversations, id)
	return nil
}

type resumeAdapter struct {
	configs []SessionConfig
	fresh   *fakeSession
}

func (*resumeAdapter) ID() string { return "resume" }

func (a *resumeAdapter) Status(context.Context) Status {
	return Status{ID: a.ID(), Name: "Resume", Available: true, Authenticated: true}
}

func (a *resumeAdapter) Start(_ context.Context, config SessionConfig) (Session, error) {
	a.configs = append(a.configs, config)
	if config.ResumeCursor != "" {
		return &staleResumeSession{cursor: config.ResumeCursor}, nil
	}
	a.fresh = &fakeSession{}
	return a.fresh, nil
}

type staleResumeSession struct {
	cursor string
	closed bool
}

func (*staleResumeSession) Send(context.Context, Turn, func(Event) error) error {
	return errors.New("provider session no longer exists")
}
func (*staleResumeSession) Interrupt(context.Context) error { return nil }
func (s *staleResumeSession) Close() error {
	s.closed = true
	return nil
}
func (s *staleResumeSession) ResumeCursor() string { return s.cursor }
func (*staleResumeSession) Restored() bool         { return true }

func TestManagerFallsBackToDurableTranscriptWhenResumeCursorIsStale(t *testing.T) {
	store := &memoryConversationStore{conversations: map[string]Conversation{
		"saved": {
			ID:           "saved",
			Title:        "Saved conversation",
			Provider:     "resume",
			ResumeCursor: "stale-cursor",
			Messages: []Message{
				{Role: RoleUser, Text: "Earlier question"},
				{Role: RoleAssistant, Text: "Earlier answer"},
			},
		},
	}}
	adapter := &resumeAdapter{}
	manager := NewManager("", "", "", adapter).UsePersistence(store)
	defer manager.Close()

	if err := manager.Send(context.Background(), TurnRequest{
		ConversationID: "saved",
		Message:        "Continue",
	}, func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(adapter.configs) != 2 || adapter.configs[0].ResumeCursor != "stale-cursor" || adapter.configs[1].ResumeCursor != "" {
		t.Fatalf("resume attempts = %#v", adapter.configs)
	}
	if adapter.fresh == nil || len(adapter.fresh.turns) != 1 || len(adapter.fresh.turns[0].History) != 2 {
		t.Fatalf("fresh transcript replay = %#v", adapter.fresh)
	}
	if got := store.conversations["saved"].ResumeCursor; got != "" {
		t.Fatalf("stale resume cursor was not cleared: %q", got)
	}
	if got := store.conversations["saved"].MessageCount; got != 4 {
		t.Fatalf("persisted message count = %d, want 4", got)
	}
}

func TestManagerReapsIdleProviderProcessButKeepsDurableConversation(t *testing.T) {
	store := &memoryConversationStore{}
	adapter := &fakeAdapter{id: "test"}
	manager := NewManager("", "", "", adapter).UsePersistence(store)
	defer manager.Close()

	var conversationID string
	if err := manager.Send(context.Background(), TurnRequest{
		Provider: "test",
		Message:  "Persist me",
	}, func(event Event) error {
		if event.Type == EventMeta {
			conversationID = event.ConversationID
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	manager.mu.RLock()
	conversation := manager.conversations[conversationID]
	manager.mu.RUnlock()
	conversation.lastUsed.Store(time.Now().Add(-time.Hour).UnixNano())
	if reaped := manager.reapIdle(time.Now().Add(-30 * time.Minute)); reaped != 1 {
		t.Fatalf("reaped %d sessions, want 1", reaped)
	}
	if !adapter.sessions[0].closed {
		t.Fatal("idle provider process was not closed")
	}
	if _, err := store.GetConversation(context.Background(), conversationID); err != nil {
		t.Fatalf("durable conversation was lost: %v", err)
	}
}
