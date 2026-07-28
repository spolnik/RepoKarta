// Package agent defines RepoKarta's provider-neutral conversation harness.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spolnik/RepoKarta/internal/access"
	"github.com/spolnik/RepoKarta/internal/contextscope"
)

// EventType identifies one streamed conversation event.
type EventType string

const (
	EventMeta        EventType = "meta"
	EventActivity    EventType = "activity"
	EventDelta       EventType = "delta"
	EventDone        EventType = "done"
	EventError       EventType = "error"
	EventSources     EventType = "sources"
	EventImages      EventType = "images"
	EventContext     EventType = "context"
	EventInterrupted EventType = "interrupted"
	EventUsage       EventType = "usage"
)

const (
	// ActivityThinking means the provider is doing work before its next visible
	// assistant message. It is a lifecycle signal, not hidden chain-of-thought.
	ActivityThinking = "thinking"
)

var (
	// ErrInterrupted means the user intentionally stopped the active turn.
	ErrInterrupted = errors.New("turn interrupted")
	// ErrNoActiveTurn means a conversation exists but is currently idle.
	ErrNoActiveTurn = errors.New("conversation has no active turn")
	// ErrConversationNotFound means no live or durable conversation exists.
	ErrConversationNotFound = errors.New("conversation is no longer active")
	// ErrConversationForbidden means the authenticated author does not own the
	// requested conversation and is not an administrator.
	ErrConversationForbidden = errors.New("conversation belongs to another author")
	// ErrInvalidInput classifies validation failures without requiring HTTP
	// handlers to match human-readable error strings.
	ErrInvalidInput = errors.New("invalid conversation input")
)

const (
	MinimumTurnTimeoutSeconds = 300
	DefaultTurnTimeoutSeconds = 1_800
	MaximumTurnTimeoutSeconds = 3_600
	DefaultTokenBudget        = 12_000
	MaximumTokenBudget        = 64_000
)

// Citation is an exact source reference observed during an MCP tool call.
type Citation struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// ContextUsage is the provider-reported context window utilization.
type ContextUsage struct {
	UsedTokens int64   `json:"used_tokens"`
	MaxTokens  int64   `json:"max_tokens"`
	Percentage float64 `json:"percentage"`
	Model      string  `json:"model,omitempty"`
}

// Usage is provider-reported billable work for a turn.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	BudgetTokens int64 `json:"budget_tokens,omitempty"`
}

// Event is emitted while a provider handles a turn.
type Event struct {
	Type           EventType     `json:"type"`
	ConversationID string        `json:"conversation_id,omitempty"`
	Title          string        `json:"title,omitempty"`
	Activity       string        `json:"activity,omitempty"`
	SegmentID      string        `json:"segment_id,omitempty"`
	Text           string        `json:"text,omitempty"`
	Sources        []Citation    `json:"sources,omitempty"`
	Images         []Image       `json:"images,omitempty"`
	Context        *ContextUsage `json:"context,omitempty"`
	Usage          *Usage        `json:"usage,omitempty"`
}

// ModelOption is one curated model exposed by a provider harness. ID is sent
// to the harness while Label is the stable human-readable name shown in the UI.
type ModelOption struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Efforts []string `json:"efforts"`
}

// Status describes whether a local provider harness is usable.
type Status struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Available     bool          `json:"available"`
	Authenticated bool          `json:"authenticated"`
	Detail        string        `json:"detail,omitempty"`
	Models        []ModelOption `json:"models,omitempty"`
	Efforts       []string      `json:"efforts,omitempty"`
	ImageInput    bool          `json:"image_input"`
	ImageOutput   bool          `json:"image_output"`
	Interrupt     bool          `json:"interrupt"`
	ContextUsage  bool          `json:"context_usage"`
	TokenUsage    bool          `json:"token_usage"`
	TokenBudget   bool          `json:"token_budget"`
}

// SessionConfig is shared by all provider adapters.
type SessionConfig struct {
	ConversationID string
	Model          string
	Effort         string
	RepositoryRoot string
	MCPURL         string
	MCPToken       string
	ResumeCursor   string
}

// Session is one provider-owned conversation.
type Session interface {
	Send(context.Context, Turn, func(Event) error) error
	Interrupt(context.Context) error
	Close() error
}

// ResumableSession exposes a provider-owned opaque cursor after startup or a
// turn. RepoKarta persists it separately from the transcript.
type ResumableSession interface {
	ResumeCursor() string
}

// RestoredSession reports whether startup recovered the provider-native
// context represented by ResumeCursor.
type RestoredSession interface {
	Restored() bool
}

// Adapter starts conversations against one installed provider harness.
type Adapter interface {
	ID() string
	Status(context.Context) Status
	Start(context.Context, SessionConfig) (Session, error)
}

// TurnRequest starts or continues a conversation.
type TurnRequest struct {
	ConversationID     string                  `json:"conversation_id"`
	Provider           string                  `json:"provider"`
	Model              string                  `json:"model"`
	Effort             string                  `json:"effort"`
	Message            string                  `json:"message"`
	Images             []Image                 `json:"images,omitempty"`
	ContextSelectors   []contextscope.Selector `json:"contexts,omitempty"`
	NamedContextIDs    []string                `json:"named_context_ids,omitempty"`
	UseDefaultContexts *bool                   `json:"use_default_contexts,omitempty"`
	TimeoutSeconds     int                     `json:"timeout_seconds,omitempty"`
	TokenBudget        int64                   `json:"token_budget,omitempty"`
	ResumeCursor       string                  `json:"-"`
	Author             ConversationAuthor      `json:"-"`
	Contexts           []contextscope.Context  `json:"-"`
}

// Turn is the provider-neutral content sent to one local harness.
type Turn struct {
	Message     string
	Images      []Image
	History     []Message
	Contexts    []contextscope.Context
	TokenBudget int64
}

// EphemeralResult is the complete output of a provider turn that is not added
// to the durable conversation list. It is used by repository-owned background
// work such as knowledge-page generation.
type EphemeralResult struct {
	Provider     string     `json:"provider"`
	Model        string     `json:"model,omitempty"`
	Text         string     `json:"text"`
	Sources      []Citation `json:"sources"`
	InputTokens  int64      `json:"input_tokens"`
	OutputTokens int64      `json:"output_tokens"`
}

type managedConversation struct {
	title      string
	author     ConversationAuthor
	provider   string
	model      string
	effort     string
	imageInput bool
	sessionMu  sync.RWMutex
	session    Session
	mu         sync.Mutex
	active     atomic.Bool
	lastUsed   atomic.Int64
	rehydrate  bool
	history    []Message
}

func (c *managedConversation) currentSession() Session {
	c.sessionMu.RLock()
	defer c.sessionMu.RUnlock()
	return c.session
}

func (c *managedConversation) replaceSession(session Session) {
	c.sessionMu.Lock()
	c.session = session
	c.sessionMu.Unlock()
}

// Manager owns live provider sessions and optionally durable RepoKarta
// transcripts. Provider-native session identifiers are opaque resume cursors
// stored separately from user-visible messages.
type Manager struct {
	mu                 sync.RWMutex
	adapters           map[string]Adapter
	conversations      map[string]*managedConversation
	conversationStarts map[string]*sync.Mutex
	ephemeralAuthors   map[string]ConversationAuthor
	repositoryRoot     string
	mcpURL             string
	mcpToken           string
	mcpTokenIssuer     MCPTokenIssuer
	citations          CitationSource
	persistence        ConversationStore
}

// MCPTokenIssuer creates and revokes credentials bound to one conversation.
type MCPTokenIssuer interface {
	Issue(string, ConversationAuthor) (string, error)
	Revoke(string)
}

// CitationSource records the exact source URLs returned to provider tools.
type CitationSource interface {
	List(string) []Citation
	Clear(string)
}

// NewManager constructs a conversation manager.
func NewManager(repositoryRoot, mcpURL, mcpToken string, adapters ...Adapter) *Manager {
	byID := make(map[string]Adapter, len(adapters))
	for _, adapter := range adapters {
		byID[adapter.ID()] = adapter
	}
	return &Manager{
		adapters:           byID,
		conversations:      make(map[string]*managedConversation),
		conversationStarts: make(map[string]*sync.Mutex),
		ephemeralAuthors:   make(map[string]ConversationAuthor),
		repositoryRoot:     repositoryRoot,
		mcpURL:             mcpURL,
		mcpToken:           mcpToken,
	}
}

// UseCitations attaches the MCP citation recorder used for authoritative source
// events. It must be configured before conversations are accepted.
func (m *Manager) UseCitations(source CitationSource) *Manager {
	m.citations = source
	return m
}

// UsePersistence enables durable titled conversations. Without it, the
// manager retains its original process-local behavior.
func (m *Manager) UsePersistence(store ConversationStore) *Manager {
	m.persistence = store
	return m
}

// UseMCPTokenIssuer replaces the process-wide provider credential with
// independently revocable conversation credentials.
func (m *Manager) UseMCPTokenIssuer(issuer MCPTokenIssuer) *Manager {
	m.mcpTokenIssuer = issuer
	return m
}

// AuthorForMCP resolves the identity bound to an active provider tool session.
// Ephemeral Wiki generation is intentionally absent from durable history, so
// its short-lived authorization lives only for the duration of the turn.
func (m *Manager) AuthorForMCP(conversationID string) (ConversationAuthor, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if author, ok := m.ephemeralAuthors[conversationID]; ok {
		return author, true
	}
	return ConversationAuthor{}, false
}

// Statuses checks all configured adapters.
func (m *Manager) Statuses(ctx context.Context) []Status {
	m.mu.RLock()
	adapters := make([]Adapter, 0, len(m.adapters))
	for _, adapter := range m.adapters {
		adapters = append(adapters, adapter)
	}
	m.mu.RUnlock()

	statuses := make([]Status, 0, len(adapters))
	for _, adapter := range adapters {
		statuses = append(statuses, adapter.Status(ctx))
	}
	sort.Slice(statuses, func(left, right int) bool {
		return statuses[left].ID < statuses[right].ID
	})
	return statuses
}

// ListConversations returns newest-first durable chat summaries.
func (m *Manager) ListConversations(ctx context.Context, filter ConversationFilter) ([]Conversation, error) {
	if m.persistence == nil {
		return []Conversation{}, nil
	}
	return m.persistence.ListConversations(ctx, filter)
}

// GetConversation loads one durable transcript.
func (m *Manager) GetConversation(ctx context.Context, id string) (Conversation, error) {
	if m.persistence == nil {
		return Conversation{}, ErrConversationNotFound
	}
	conversation, err := m.persistence.GetConversation(ctx, id)
	return conversation, err
}

// RenameConversation changes a durable chat title.
func (m *Manager) RenameConversation(ctx context.Context, id, title string) error {
	if m.persistence == nil {
		return ErrConversationNotFound
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("%w: conversation title is required", ErrInvalidInput)
	}
	if len(title) > 120 {
		return fmt.Errorf("%w: conversation title exceeds 120 characters", ErrInvalidInput)
	}
	if err := m.persistence.RenameConversation(ctx, id, title); err != nil {
		return err
	}
	m.mu.RLock()
	conversation := m.conversations[id]
	m.mu.RUnlock()
	if conversation != nil {
		conversation.mu.Lock()
		conversation.title = strings.TrimSpace(title)
		conversation.mu.Unlock()
	}
	return nil
}

// DeleteConversation closes any live provider process and deletes the durable
// RepoKarta transcript and its exact owned image files.
func (m *Manager) DeleteConversation(ctx context.Context, id string) error {
	if m.persistence == nil {
		return ErrConversationNotFound
	}
	m.mu.Lock()
	conversation := m.conversations[id]
	delete(m.conversations, id)
	m.mu.Unlock()
	if conversation != nil {
		if conversation.active.Load() {
			m.mu.Lock()
			m.conversations[id] = conversation
			m.mu.Unlock()
			return errors.New("cannot delete a conversation while a turn is active")
		}
		_ = conversation.currentSession().Close()
	}
	if m.mcpTokenIssuer != nil {
		m.mcpTokenIssuer.Revoke(id)
	}
	if m.citations != nil {
		m.citations.Clear(id)
	}
	return m.persistence.DeleteConversation(ctx, id)
}

// Send streams a single turn. New conversation IDs are generated server-side.
func (m *Manager) Send(ctx context.Context, request TurnRequest, emit func(Event) error) error {
	if request.Message == "" && len(request.Images) == 0 {
		return fmt.Errorf("%w: message or image is required", ErrInvalidInput)
	}
	if err := ValidateImages(request.Images); err != nil {
		return err
	}
	timeoutSeconds, tokenBudget, err := normalizeTurnControls(request.TimeoutSeconds, request.TokenBudget)
	if err != nil {
		return err
	}

	conversation, conversationID, err := m.conversation(ctx, request)
	if err != nil {
		return err
	}
	if !conversation.active.CompareAndSwap(false, true) {
		return errors.New("conversation already has an active turn")
	}
	defer conversation.active.Store(false)
	m.mu.RLock()
	currentConversation := m.conversations[conversationID]
	m.mu.RUnlock()
	if currentConversation != conversation {
		return ErrConversationNotFound
	}
	conversation.lastUsed.Store(time.Now().UTC().UnixNano())
	if err := emit(Event{Type: EventMeta, ConversationID: conversationID, Title: conversation.title}); err != nil {
		m.dropConversation(conversationID, conversation)
		return err
	}

	conversation.mu.Lock()
	defer conversation.mu.Unlock()
	turnContext, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	userMessage := Message{
		ConversationID: conversationID,
		Role:           RoleUser,
		Text:           request.Message,
		Images:         append([]Image(nil), request.Images...),
		Contexts:       append([]contextscope.Context(nil), request.Contexts...),
		Status:         "complete",
		CreatedAt:      time.Now().UTC(),
	}
	if m.persistence != nil {
		userMessage, err = m.persistence.AppendMessage(turnContext, userMessage)
		if err != nil {
			return fmt.Errorf("persist user message: %w", err)
		}
	}
	history := []Message(nil)
	if conversation.rehydrate {
		history = append(history, conversation.history...)
	}
	assistantMessage := Message{
		ConversationID: conversationID,
		Role:           RoleAssistant,
		Status:         "complete",
		CreatedAt:      time.Now().UTC(),
	}
	providerOutputObserved := false
	lastSegmentID := ""
	forward := func(event Event) error {
		switch event.Type {
		case EventDelta:
			providerOutputObserved = true
			if event.SegmentID != "" && lastSegmentID != "" && event.SegmentID != lastSegmentID {
				assistantMessage.Text = strings.TrimRight(assistantMessage.Text, "\n") + "\n\n"
			}
			assistantMessage.Text += event.Text
			if event.SegmentID != "" {
				lastSegmentID = event.SegmentID
			}
		case EventImages:
			providerOutputObserved = true
			assistantMessage.Images = append(assistantMessage.Images, event.Images...)
		case EventSources:
			providerOutputObserved = true
			assistantMessage.Sources = append([]Citation(nil), event.Sources...)
		case EventContext:
			providerOutputObserved = true
		case EventUsage:
			providerOutputObserved = true
			if event.Usage != nil {
				assistantMessage.InputTokens = event.Usage.InputTokens
				assistantMessage.OutputTokens = event.Usage.OutputTokens
			}
		}
		return emit(event)
	}
	if m.citations != nil {
		m.citations.Clear(conversationID)
	}
	activeSession := conversation.currentSession()
	if err := forward(Event{Type: EventActivity, Activity: ActivityThinking}); err != nil {
		return err
	}
	sendError := activeSession.Send(turnContext, Turn{
		Message:     request.Message,
		Images:      request.Images,
		History:     history,
		Contexts:    request.Contexts,
		TokenBudget: tokenBudget,
	}, forward)
	if sendError != nil && !providerOutputObserved {
		if restored, ok := activeSession.(RestoredSession); ok && restored.Restored() {
			staleSession := activeSession
			freshRequest := request
			freshRequest.Provider = conversation.provider
			freshRequest.Model = conversation.model
			freshRequest.Effort = conversation.effort
			freshRequest.ResumeCursor = ""
			freshConversation, restartError := m.startConversation(turnContext, freshRequest, conversationID)
			if m.persistence != nil {
				_ = m.persistence.UpdateConversationCursor(context.WithoutCancel(ctx), conversationID, "")
			}
			if restartError == nil {
				activeSession = freshConversation.currentSession()
				conversation.replaceSession(activeSession)
				conversation.imageInput = freshConversation.imageInput
				conversation.rehydrate = len(conversation.history) > 0
				history = append([]Message(nil), conversation.history...)
				_ = staleSession.Close()
				if m.citations != nil {
					m.citations.Clear(conversationID)
				}
				sendError = activeSession.Send(turnContext, Turn{
					Message:     request.Message,
					Images:      request.Images,
					History:     history,
					Contexts:    request.Contexts,
					TokenBudget: tokenBudget,
				}, forward)
			} else {
				sendError = errors.Join(sendError, fmt.Errorf("start transcript replay fallback: %w", restartError))
			}
		}
	}
	switch {
	case errors.Is(sendError, ErrInterrupted):
		assistantMessage.Status = "interrupted"
	case errors.Is(sendError, context.DeadlineExceeded), errors.Is(turnContext.Err(), context.DeadlineExceeded):
		assistantMessage.Status = "timeout"
		assistantMessage.Error = fmt.Sprintf("turn exceeded the %d second timeout", timeoutSeconds)
	case sendError != nil:
		assistantMessage.Status = "error"
		assistantMessage.Error = sendError.Error()
	}
	if m.citations != nil && sendError == nil {
		sources := m.citations.List(conversationID)
		if len(sources) > 0 {
			assistantMessage.Sources = append([]Citation(nil), sources...)
			if err := emit(Event{Type: EventSources, ConversationID: conversationID, Sources: sources}); err != nil {
				m.dropConversation(conversationID, conversation)
				return err
			}
		}
		m.citations.Clear(conversationID)
	}
	if m.persistence != nil &&
		(assistantMessage.Text != "" || len(assistantMessage.Images) > 0 ||
			len(assistantMessage.Sources) > 0 || assistantMessage.Status != "complete") {
		if _, persistError := m.persistence.AppendMessage(context.WithoutCancel(ctx), assistantMessage); persistError != nil {
			return fmt.Errorf("persist assistant message: %w", persistError)
		}
	}
	if m.persistence != nil {
		if resumable, ok := activeSession.(ResumableSession); ok {
			if cursor := strings.TrimSpace(resumable.ResumeCursor()); cursor != "" {
				if cursorError := m.persistence.UpdateConversationCursor(
					context.WithoutCancel(ctx),
					conversationID,
					cursor,
				); cursorError != nil {
					return fmt.Errorf("persist provider resume cursor: %w", cursorError)
				}
			}
		}
	}
	conversation.history = append(conversation.history, userMessage)
	if assistantMessage.Text != "" || len(assistantMessage.Images) > 0 ||
		len(assistantMessage.Sources) > 0 || assistantMessage.Status != "complete" {
		conversation.history = append(conversation.history, assistantMessage)
	}
	conversation.rehydrate = false
	conversation.lastUsed.Store(time.Now().UTC().UnixNano())

	if errors.Is(sendError, ErrInterrupted) {
		if m.citations != nil {
			m.citations.Clear(conversationID)
		}
		return emit(Event{Type: EventInterrupted, ConversationID: conversationID})
	}
	if assistantMessage.Status == "timeout" {
		m.dropConversation(conversationID, conversation)
		return errors.New(assistantMessage.Error)
	}
	if sendError != nil {
		m.dropConversation(conversationID, conversation)
		return m.providerSessionError(ctx, conversation.provider, sendError)
	}
	if err := emit(Event{Type: EventDone, ConversationID: conversationID}); err != nil {
		m.dropConversation(conversationID, conversation)
		return err
	}
	return nil
}

// RunEphemeral executes one isolated provider turn without creating durable
// conversation metadata or retaining a resumable provider session.
func (m *Manager) RunEphemeral(
	ctx context.Context,
	request TurnRequest,
	emit func(Event) error,
) (EphemeralResult, error) {
	if request.Message == "" && len(request.Images) == 0 {
		return EphemeralResult{}, fmt.Errorf("%w: message or image is required", ErrInvalidInput)
	}
	if err := ValidateImages(request.Images); err != nil {
		return EphemeralResult{}, err
	}
	timeoutSeconds, tokenBudget, err := normalizeTurnControls(request.TimeoutSeconds, request.TokenBudget)
	if err != nil {
		return EphemeralResult{}, err
	}
	if emit == nil {
		emit = func(Event) error { return nil }
	}
	if viewer, ok := access.ViewerFromContext(ctx); ok {
		request.Author.ID = viewer.ID
		request.Author.Groups = append([]string(nil), viewer.Groups...)
	}
	request.Author = normalizeConversationAuthor(request.Author)
	conversationID, err := randomID()
	if err != nil {
		return EphemeralResult{}, fmt.Errorf("create ephemeral turn id: %w", err)
	}
	m.mu.Lock()
	m.ephemeralAuthors[conversationID] = request.Author
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.ephemeralAuthors, conversationID)
		m.mu.Unlock()
		if m.mcpTokenIssuer != nil {
			m.mcpTokenIssuer.Revoke(conversationID)
		}
	}()
	conversation, err := m.startConversation(ctx, request, conversationID)
	if err != nil {
		return EphemeralResult{}, err
	}
	session := conversation.currentSession()
	defer session.Close()

	result := EphemeralResult{
		Provider: conversation.provider,
		Model:    conversation.model,
		Sources:  []Citation{},
	}
	turnContext, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	if m.citations != nil {
		m.citations.Clear(conversationID)
		defer m.citations.Clear(conversationID)
	}
	lastSegmentID := ""
	forward := func(event Event) error {
		switch event.Type {
		case EventDelta:
			if event.SegmentID != "" && lastSegmentID != "" && event.SegmentID != lastSegmentID {
				result.Text = ""
			}
			result.Text += event.Text
			if event.SegmentID != "" {
				lastSegmentID = event.SegmentID
			}
		case EventSources:
			result.Sources = append([]Citation(nil), event.Sources...)
		case EventUsage:
			if event.Usage != nil {
				result.InputTokens = event.Usage.InputTokens
				result.OutputTokens = event.Usage.OutputTokens
			}
		}
		return emit(event)
	}
	if err := forward(Event{
		Type:           EventMeta,
		ConversationID: conversationID,
		Title:          "Ephemeral generation",
	}); err != nil {
		return result, err
	}
	if err := forward(Event{Type: EventActivity, Activity: ActivityThinking}); err != nil {
		return result, err
	}
	sendError := session.Send(turnContext, Turn{
		Message:     request.Message,
		Images:      request.Images,
		Contexts:    request.Contexts,
		TokenBudget: tokenBudget,
	}, forward)
	if sendError == nil && m.citations != nil {
		result.Sources = m.citations.List(conversationID)
		if len(result.Sources) > 0 {
			if err := emit(Event{
				Type:           EventSources,
				ConversationID: conversationID,
				Sources:        result.Sources,
			}); err != nil {
				return result, err
			}
		}
	}
	if errors.Is(sendError, context.DeadlineExceeded) ||
		errors.Is(turnContext.Err(), context.DeadlineExceeded) {
		return result, fmt.Errorf("turn exceeded the %d second timeout", timeoutSeconds)
	}
	if sendError != nil {
		return result, m.providerSessionError(ctx, conversation.provider, sendError)
	}
	if err := emit(Event{Type: EventDone, ConversationID: conversationID}); err != nil {
		return result, err
	}
	return result, nil
}

// Interrupt stops the active provider turn while preserving its conversation.
func (m *Manager) Interrupt(ctx context.Context, conversationID string) error {
	m.mu.RLock()
	conversation := m.conversations[conversationID]
	m.mu.RUnlock()
	if conversation == nil {
		return ErrConversationNotFound
	}
	if !conversation.active.Load() {
		return ErrNoActiveTurn
	}
	return conversation.currentSession().Interrupt(ctx)
}

func (m *Manager) conversation(ctx context.Context, request TurnRequest) (*managedConversation, string, error) {
	request.Author = normalizeConversationAuthor(request.Author)
	if request.ConversationID != "" {
		startLock := m.conversationStartLock(request.ConversationID)
		startLock.Lock()
		defer startLock.Unlock()
		m.mu.RLock()
		conversation := m.conversations[request.ConversationID]
		m.mu.RUnlock()
		if conversation == nil && m.persistence != nil {
			stored, err := m.persistence.GetConversation(ctx, request.ConversationID)
			if err != nil {
				return nil, "", ErrConversationNotFound
			}
			stored.Author = normalizeConversationAuthor(stored.Author)
			if stored.Author.ID != request.Author.ID {
				return nil, "", ErrConversationForbidden
			}
			request.Provider = stored.Provider
			request.Model = stored.Model
			request.Effort = stored.Effort
			request.ResumeCursor = stored.ResumeCursor
			conversation, err = m.startConversation(ctx, request, request.ConversationID)
			if err != nil {
				return nil, "", err
			}
			conversation.title = stored.Title
			conversation.author = stored.Author
			conversation.history = append([]Message(nil), stored.Messages...)
			conversation.rehydrate = len(stored.Messages) > 0
			if restored, ok := conversation.currentSession().(RestoredSession); ok && restored.Restored() {
				conversation.rehydrate = false
			}
			m.mu.Lock()
			if existing := m.conversations[request.ConversationID]; existing != nil {
				m.mu.Unlock()
				_ = conversation.currentSession().Close()
				conversation = existing
			} else {
				m.conversations[request.ConversationID] = conversation
				m.mu.Unlock()
			}
		}
		if conversation == nil {
			return nil, "", ErrConversationNotFound
		}
		if conversation.author.ID != request.Author.ID {
			return nil, "", ErrConversationForbidden
		}
		if request.Provider != "" && request.Provider != conversation.provider {
			return nil, "", fmt.Errorf("%w: a conversation cannot switch providers", ErrInvalidInput)
		}
		if request.Model != "" && request.Model != conversation.model {
			return nil, "", fmt.Errorf("%w: a conversation cannot switch models", ErrInvalidInput)
		}
		if request.Effort != "" && request.Effort != conversation.effort {
			return nil, "", fmt.Errorf("%w: a conversation cannot switch effort", ErrInvalidInput)
		}
		if len(request.Images) > 0 && !conversation.imageInput {
			return nil, "", fmt.Errorf("%w: this provider does not support image input", ErrInvalidInput)
		}
		// Refresh IdP groups from the newly authenticated browser request before
		// the provider can make conversation-scoped MCP calls. This prevents a
		// resumed conversation from retaining stale group membership.
		if conversation.author.ID == request.Author.ID {
			conversation.author = request.Author
			if m.persistence != nil {
				if err := m.persistence.UpdateConversationAuthor(ctx, request.ConversationID, request.Author); err != nil {
					return nil, "", fmt.Errorf("refresh conversation author: %w", err)
				}
			}
		}
		return conversation, request.ConversationID, nil
	}

	conversationID, err := randomID()
	if err != nil {
		return nil, "", fmt.Errorf("create conversation id: %w", err)
	}
	conversation, err := m.startConversation(ctx, request, conversationID)
	if err != nil {
		return nil, "", err
	}
	conversation.title = DefaultConversationTitle(request.Message, request.Images)
	conversation.author = request.Author
	if m.persistence != nil {
		now := time.Now().UTC()
		if err := m.persistence.CreateConversation(ctx, Conversation{
			ID:        conversationID,
			Title:     conversation.title,
			Provider:  conversation.provider,
			Model:     conversation.model,
			Effort:    conversation.effort,
			Author:    conversation.author,
			CreatedAt: now,
			UpdatedAt: now,
		}); err != nil {
			_ = conversation.currentSession().Close()
			return nil, "", fmt.Errorf("persist conversation: %w", err)
		}
	}

	m.mu.Lock()
	m.conversations[conversationID] = conversation
	m.mu.Unlock()
	return conversation, conversationID, nil
}

func (m *Manager) conversationStartLock(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock := m.conversationStarts[id]
	if lock == nil {
		lock = &sync.Mutex{}
		m.conversationStarts[id] = lock
	}
	return lock
}

func normalizeConversationAuthor(author ConversationAuthor) ConversationAuthor {
	author.ID = strings.TrimSpace(author.ID)
	author.Name = strings.TrimSpace(author.Name)
	author.Email = strings.TrimSpace(author.Email)
	author.Provider = strings.TrimSpace(author.Provider)
	if author.Provider == "" {
		author.Provider = "local"
	}
	if author.ID == "" {
		author.ID = "local:admin"
	}
	if author.Name == "" && author.Email == "" && author.ID == "local:admin" {
		author.Name = "Local administrator"
	}
	return author
}

func (m *Manager) startConversation(ctx context.Context, request TurnRequest, conversationID string) (*managedConversation, error) {
	adapter := m.adapter(request.Provider)
	if adapter == nil {
		return nil, fmt.Errorf("unknown provider %q", request.Provider)
	}
	status := adapter.Status(ctx)
	if !status.Available {
		return nil, fmt.Errorf("%s is not available: %s", status.Name, status.Detail)
	}
	if !status.Authenticated {
		return nil, fmt.Errorf("%s is not authenticated in RepoKarta's launch context: %s", status.Name, status.Detail)
	}
	if len(request.Images) > 0 && !status.ImageInput {
		return nil, fmt.Errorf("%s does not support image input", status.Name)
	}
	if len(status.Models) > 0 {
		if strings.TrimSpace(request.Model) == "" {
			request.Model = status.Models[0].ID
		}
		if !containsModel(status.Models, request.Model) {
			return nil, fmt.Errorf("%s does not support model %q", status.Name, request.Model)
		}
	}
	supportedEfforts := status.Efforts
	for _, model := range status.Models {
		if model.ID == request.Model && model.Efforts != nil {
			supportedEfforts = model.Efforts
			break
		}
	}
	if request.Effort != "" && !contains(supportedEfforts, request.Effort) {
		return nil, fmt.Errorf(
			"%s model %q does not support effort %q",
			status.Name,
			request.Model,
			request.Effort,
		)
	}
	mcpToken := m.mcpToken
	if m.mcpTokenIssuer != nil {
		issuedToken, issueErr := m.mcpTokenIssuer.Issue(conversationID, normalizeConversationAuthor(request.Author))
		if issueErr != nil {
			return nil, fmt.Errorf("issue conversation MCP credential: %w", issueErr)
		}
		mcpToken = issuedToken
	}
	session, err := adapter.Start(ctx, SessionConfig{
		ConversationID: conversationID,
		Model:          request.Model,
		Effort:         request.Effort,
		RepositoryRoot: m.repositoryRoot,
		MCPURL:         conversationMCPURL(m.mcpURL, conversationID),
		MCPToken:       mcpToken,
		ResumeCursor:   request.ResumeCursor,
	})
	if err != nil {
		if m.mcpTokenIssuer != nil {
			m.mcpTokenIssuer.Revoke(conversationID)
		}
		probeContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		refreshed := adapter.Status(probeContext)
		cancel()
		switch {
		case !refreshed.Available:
			return nil, fmt.Errorf("%s became unavailable in RepoKarta's launch context: %s", status.Name, refreshed.Detail)
		case !refreshed.Authenticated:
			return nil, fmt.Errorf("%s is not authenticated in RepoKarta's launch context: %s", status.Name, refreshed.Detail)
		default:
			return nil, fmt.Errorf("start %s in RepoKarta's launch context: %w", status.Name, err)
		}
	}
	conversation := &managedConversation{
		provider:   adapter.ID(),
		model:      request.Model,
		effort:     request.Effort,
		imageInput: status.ImageInput,
		session:    session,
	}
	conversation.lastUsed.Store(time.Now().UTC().UnixNano())
	return conversation, nil
}

func (m *Manager) providerSessionError(ctx context.Context, providerID string, sessionError error) error {
	if errors.Is(sessionError, context.Canceled) || errors.Is(sessionError, context.DeadlineExceeded) {
		return sessionError
	}
	adapter := m.adapter(providerID)
	if adapter == nil {
		return sessionError
	}
	probeContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	status := adapter.Status(probeContext)
	cancel()
	switch {
	case !status.Available:
		return fmt.Errorf("%s became unavailable in RepoKarta's launch context: %s", status.Name, status.Detail)
	case !status.Authenticated:
		return fmt.Errorf("%s is not authenticated in RepoKarta's launch context: %s", status.Name, status.Detail)
	default:
		return sessionError
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func conversationMCPURL(endpoint, conversationID string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	query := parsed.Query()
	query.Set("conversation_id", conversationID)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func (m *Manager) dropConversation(id string, conversation *managedConversation) {
	m.mu.Lock()
	if m.conversations[id] == conversation {
		delete(m.conversations, id)
	}
	m.mu.Unlock()
	_ = conversation.currentSession().Close()
	if m.mcpTokenIssuer != nil {
		m.mcpTokenIssuer.Revoke(id)
	}
	if m.citations != nil {
		m.citations.Clear(id)
	}
}

func (m *Manager) adapter(id string) Adapter {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.adapters[id]
}

// StartIdleReaper lets provider processes remain ephemeral while durable
// RepoKarta transcripts and resume cursors remain available. A reopened chat
// first resumes the provider-native session, then falls back to transcript
// replay if that cursor is stale.
func (m *Manager) StartIdleReaper(ctx context.Context, idle time.Duration) {
	if idle <= 0 {
		return
	}
	interval := idle / 6
	if interval < time.Minute {
		interval = time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				m.reapIdle(now.Add(-idle))
			}
		}
	}()
}

func (m *Manager) reapIdle(cutoff time.Time) int {
	m.mu.Lock()
	expired := make(map[string]*managedConversation)
	for id, conversation := range m.conversations {
		lastUsed := time.Unix(0, conversation.lastUsed.Load())
		if conversation.active.Load() || !lastUsed.Before(cutoff) {
			continue
		}
		delete(m.conversations, id)
		expired[id] = conversation
	}
	m.mu.Unlock()
	for id, conversation := range expired {
		_ = conversation.currentSession().Close()
		if m.mcpTokenIssuer != nil {
			m.mcpTokenIssuer.Revoke(id)
		}
	}
	return len(expired)
}

// Close releases every running provider subprocess.
func (m *Manager) Close() error {
	m.mu.Lock()
	conversations := m.conversations
	m.conversations = make(map[string]*managedConversation)
	m.mu.Unlock()

	var closeError error
	for id, conversation := range conversations {
		if err := conversation.currentSession().Close(); err != nil && !errors.Is(err, io.EOF) {
			closeError = errors.Join(closeError, err)
		}
		if m.mcpTokenIssuer != nil {
			m.mcpTokenIssuer.Revoke(id)
		}
	}
	return closeError
}

func randomID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func normalizeTurnControls(timeoutSeconds int, tokenBudget int64) (int, int64, error) {
	if timeoutSeconds == 0 {
		timeoutSeconds = DefaultTurnTimeoutSeconds
	}
	if timeoutSeconds < MinimumTurnTimeoutSeconds || timeoutSeconds > MaximumTurnTimeoutSeconds {
		return 0, 0, fmt.Errorf(
			"%w: timeout_seconds must be from %d to %d",
			ErrInvalidInput,
			MinimumTurnTimeoutSeconds,
			MaximumTurnTimeoutSeconds,
		)
	}
	if tokenBudget == 0 {
		tokenBudget = DefaultTokenBudget
	}
	if tokenBudget < 256 || tokenBudget > MaximumTokenBudget {
		return 0, 0, fmt.Errorf(
			"%w: token_budget must be from 256 to %d",
			ErrInvalidInput,
			MaximumTokenBudget,
		)
	}
	return timeoutSeconds, tokenBudget, nil
}

func containsModel(models []ModelOption, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, candidate := range models {
		if candidate.ID == model {
			return true
		}
	}
	return false
}
