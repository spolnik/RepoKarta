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
	"sync"
	"sync/atomic"
	"time"
)

// EventType identifies one streamed conversation event.
type EventType string

const (
	EventMeta        EventType = "meta"
	EventDelta       EventType = "delta"
	EventDone        EventType = "done"
	EventError       EventType = "error"
	EventSources     EventType = "sources"
	EventImages      EventType = "images"
	EventContext     EventType = "context"
	EventInterrupted EventType = "interrupted"
)

var (
	// ErrInterrupted means the user intentionally stopped the active turn.
	ErrInterrupted = errors.New("turn interrupted")
	// ErrNoActiveTurn means a conversation exists but is currently idle.
	ErrNoActiveTurn = errors.New("conversation has no active turn")
	// ErrConversationNotFound means the ephemeral conversation has expired.
	ErrConversationNotFound = errors.New("conversation is no longer active")
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

// Event is emitted while a provider handles a turn.
type Event struct {
	Type           EventType     `json:"type"`
	ConversationID string        `json:"conversation_id,omitempty"`
	Text           string        `json:"text,omitempty"`
	Sources        []Citation    `json:"sources,omitempty"`
	Images         []Image       `json:"images,omitempty"`
	Context        *ContextUsage `json:"context,omitempty"`
}

// Status describes whether a local provider harness is usable.
type Status struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Available        bool     `json:"available"`
	Authenticated    bool     `json:"authenticated"`
	Detail           string   `json:"detail,omitempty"`
	Models           []string `json:"models,omitempty"`
	ModelPlaceholder string   `json:"model_placeholder,omitempty"`
	Efforts          []string `json:"efforts,omitempty"`
	ImageInput       bool     `json:"image_input"`
	ImageOutput      bool     `json:"image_output"`
	Interrupt        bool     `json:"interrupt"`
	ContextUsage     bool     `json:"context_usage"`
}

// SessionConfig is shared by all provider adapters.
type SessionConfig struct {
	ConversationID string
	Model          string
	Effort         string
	RepositoryRoot string
	MCPURL         string
	MCPToken       string
}

// Session is one provider-owned conversation.
type Session interface {
	Send(context.Context, Turn, func(Event) error) error
	Interrupt(context.Context) error
	Close() error
}

// Adapter starts conversations against one installed provider harness.
type Adapter interface {
	ID() string
	Status(context.Context) Status
	Start(context.Context, SessionConfig) (Session, error)
}

// TurnRequest starts or continues a conversation.
type TurnRequest struct {
	ConversationID string  `json:"conversation_id"`
	Provider       string  `json:"provider"`
	Model          string  `json:"model"`
	Effort         string  `json:"effort"`
	Message        string  `json:"message"`
	Images         []Image `json:"images,omitempty"`
}

// Turn is the provider-neutral content sent to one local harness.
type Turn struct {
	Message string
	Images  []Image
}

type managedConversation struct {
	provider   string
	model      string
	effort     string
	imageInput bool
	session    Session
	mu         sync.Mutex
	active     atomic.Bool
}

// Manager owns ephemeral provider sessions. Conversation text is not persisted.
type Manager struct {
	mu             sync.RWMutex
	adapters       map[string]Adapter
	conversations  map[string]*managedConversation
	repositoryRoot string
	mcpURL         string
	mcpToken       string
	citations      CitationSource
}

// CitationSource records the exact source URLs returned to provider tools.
type CitationSource interface {
	List(string) []Citation
	Clear(string)
}

// NewManager constructs an ephemeral conversation manager.
func NewManager(repositoryRoot, mcpURL, mcpToken string, adapters ...Adapter) *Manager {
	byID := make(map[string]Adapter, len(adapters))
	for _, adapter := range adapters {
		byID[adapter.ID()] = adapter
	}
	return &Manager{
		adapters:       byID,
		conversations:  make(map[string]*managedConversation),
		repositoryRoot: repositoryRoot,
		mcpURL:         mcpURL,
		mcpToken:       mcpToken,
	}
}

// UseCitations attaches the MCP citation recorder used for authoritative source
// events. It must be configured before conversations are accepted.
func (m *Manager) UseCitations(source CitationSource) *Manager {
	m.citations = source
	return m
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

// Send streams a single turn. New conversation IDs are generated server-side.
func (m *Manager) Send(ctx context.Context, request TurnRequest, emit func(Event) error) error {
	if request.Message == "" && len(request.Images) == 0 {
		return errors.New("message or image is required")
	}
	if err := ValidateImages(request.Images); err != nil {
		return err
	}

	conversation, conversationID, err := m.conversation(ctx, request)
	if err != nil {
		return err
	}
	if err := emit(Event{Type: EventMeta, ConversationID: conversationID}); err != nil {
		m.dropConversation(conversationID, conversation)
		return err
	}

	conversation.mu.Lock()
	defer conversation.mu.Unlock()
	conversation.active.Store(true)
	defer conversation.active.Store(false)
	if m.citations != nil {
		m.citations.Clear(conversationID)
	}
	if err := conversation.session.Send(ctx, Turn{Message: request.Message, Images: request.Images}, emit); errors.Is(err, ErrInterrupted) {
		if m.citations != nil {
			m.citations.Clear(conversationID)
		}
		return emit(Event{Type: EventInterrupted, ConversationID: conversationID})
	} else if err != nil {
		m.dropConversation(conversationID, conversation)
		return m.providerSessionError(ctx, conversation.provider, err)
	}
	if m.citations != nil {
		sources := m.citations.List(conversationID)
		if len(sources) > 0 {
			if err := emit(Event{Type: EventSources, ConversationID: conversationID, Sources: sources}); err != nil {
				m.dropConversation(conversationID, conversation)
				return err
			}
		}
		m.citations.Clear(conversationID)
	}
	if err := emit(Event{Type: EventDone, ConversationID: conversationID}); err != nil {
		m.dropConversation(conversationID, conversation)
		return err
	}
	return nil
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
	return conversation.session.Interrupt(ctx)
}

func (m *Manager) conversation(ctx context.Context, request TurnRequest) (*managedConversation, string, error) {
	if request.ConversationID != "" {
		m.mu.RLock()
		conversation := m.conversations[request.ConversationID]
		m.mu.RUnlock()
		if conversation == nil {
			return nil, "", ErrConversationNotFound
		}
		if request.Provider != "" && request.Provider != conversation.provider {
			return nil, "", errors.New("a conversation cannot switch providers")
		}
		if request.Model != "" && request.Model != conversation.model {
			return nil, "", errors.New("a conversation cannot switch models")
		}
		if request.Effort != "" && request.Effort != conversation.effort {
			return nil, "", errors.New("a conversation cannot switch effort")
		}
		if len(request.Images) > 0 && !conversation.imageInput {
			return nil, "", errors.New("this provider does not support image input")
		}
		return conversation, request.ConversationID, nil
	}

	adapter := m.adapter(request.Provider)
	if adapter == nil {
		return nil, "", fmt.Errorf("unknown provider %q", request.Provider)
	}
	status := adapter.Status(ctx)
	if !status.Available {
		return nil, "", fmt.Errorf("%s is not available: %s", status.Name, status.Detail)
	}
	if !status.Authenticated {
		return nil, "", fmt.Errorf("%s is not authenticated in RepoKarta's launch context: %s", status.Name, status.Detail)
	}
	if len(request.Images) > 0 && !status.ImageInput {
		return nil, "", fmt.Errorf("%s does not support image input", status.Name)
	}
	if request.Effort != "" && !contains(status.Efforts, request.Effort) {
		return nil, "", fmt.Errorf("%s does not support effort %q", status.Name, request.Effort)
	}
	conversationID, err := randomID()
	if err != nil {
		return nil, "", fmt.Errorf("create conversation id: %w", err)
	}
	session, err := adapter.Start(ctx, SessionConfig{
		ConversationID: conversationID,
		Model:          request.Model,
		Effort:         request.Effort,
		RepositoryRoot: m.repositoryRoot,
		MCPURL:         conversationMCPURL(m.mcpURL, conversationID),
		MCPToken:       m.mcpToken,
	})
	if err != nil {
		probeContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		refreshed := adapter.Status(probeContext)
		cancel()
		switch {
		case !refreshed.Available:
			return nil, "", fmt.Errorf("%s became unavailable in RepoKarta's launch context: %s", status.Name, refreshed.Detail)
		case !refreshed.Authenticated:
			return nil, "", fmt.Errorf("%s is not authenticated in RepoKarta's launch context: %s", status.Name, refreshed.Detail)
		default:
			return nil, "", fmt.Errorf("start %s in RepoKarta's launch context: %w", status.Name, err)
		}
	}
	conversation := &managedConversation{
		provider:   adapter.ID(),
		model:      request.Model,
		effort:     request.Effort,
		imageInput: status.ImageInput,
		session:    session,
	}

	m.mu.Lock()
	m.conversations[conversationID] = conversation
	m.mu.Unlock()
	return conversation, conversationID, nil
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
	_ = conversation.session.Close()
	if m.citations != nil {
		m.citations.Clear(id)
	}
}

func (m *Manager) adapter(id string) Adapter {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.adapters[id]
}

// Close releases every running provider subprocess.
func (m *Manager) Close() error {
	m.mu.Lock()
	conversations := m.conversations
	m.conversations = make(map[string]*managedConversation)
	m.mu.Unlock()

	var closeError error
	for _, conversation := range conversations {
		if err := conversation.session.Close(); err != nil && !errors.Is(err, io.EOF) {
			closeError = errors.Join(closeError, err)
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
