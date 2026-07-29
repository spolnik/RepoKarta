package codework

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	StateCreating         = "creating"
	StateReady            = "ready"
	StateRunning          = "running"
	StateAwaitingApproval = "awaiting_approval"
	StateInterrupted      = "interrupted"
	StateFinishing        = "finishing"
	StateFinished         = "finished"
	StateDiscarding       = "discarding"
	StateDiscarded        = "discarded"
	StateFailed           = "failed"
)

// Session is the durable identity and lifecycle for one Code tab worktree.
type Session struct {
	ID             string    `json:"id"`
	RepositoryID   int64     `json:"repository_id"`
	Repository     string    `json:"repository"`
	ConversationID string    `json:"conversation_id,omitempty"`
	AuthorID       string    `json:"author_id"`
	Provider       string    `json:"provider"`
	Model          string    `json:"model,omitempty"`
	Effort         string    `json:"effort,omitempty"`
	Baseline       string    `json:"baseline"`
	Branch         string    `json:"branch"`
	State          string    `json:"state"`
	Version        int64     `json:"version"`
	Error          string    `json:"error,omitempty"`
	FinishedCommit string    `json:"finished_commit,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	FinishedAt     time.Time `json:"finished_at,omitempty"`
}

// Action is one bounded, visible provider or lifecycle event.
type Action struct {
	ID         int64     `json:"id"`
	SessionID  string    `json:"session_id"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	Summary    string    `json:"summary,omitempty"`
	Command    string    `json:"command,omitempty"`
	Output     string    `json:"output,omitempty"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	ApprovalID string    `json:"approval_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Approval is a single-use provider request surfaced in the Code tab.
type Approval struct {
	ID          string    `json:"id"`
	SessionID   string    `json:"session_id"`
	Kind        string    `json:"kind"`
	Reason      string    `json:"reason,omitempty"`
	Command     string    `json:"command,omitempty"`
	Directory   string    `json:"directory,omitempty"`
	Status      string    `json:"status"`
	Decision    string    `json:"decision,omitempty"`
	RequestedAt time.Time `json:"requested_at"`
	ResolvedAt  time.Time `json:"resolved_at,omitempty"`
}

// SessionStore is implemented by the selected metadata backend.
type SessionStore interface {
	CreateCodeSession(context.Context, Session) error
	CodeSession(context.Context, string) (Session, error)
	ListCodeSessions(context.Context, string) ([]Session, error)
	UpdateCodeSession(context.Context, Session) error
	RecoverActiveCodeSessions(context.Context, time.Time) (int64, error)
	DeleteCodeSession(context.Context, string) error
	AppendCodeAction(context.Context, Action) (Action, error)
	CodeActions(context.Context, string, int) ([]Action, error)
	CreateCodeApproval(context.Context, Approval) error
	CodeApproval(context.Context, string) (Approval, error)
	CodeApprovals(context.Context, string) ([]Approval, error)
	ResolveCodeApproval(context.Context, string, string, string, time.Time) error
	RepositoryCodingEnabled(context.Context, int64) (bool, error)
}

// Service coordinates durable session state with owned Git worktrees.
type Service struct {
	worktrees *Manager
	store     SessionStore
	mu        sync.Mutex
}

// CreateSessionRequest begins one authorized coding session.
type CreateSessionRequest struct {
	RepositoryID int64
	AuthorID     string
	Provider     string
	Model        string
	Effort       string
	Baseline     string
}

// NewService composes durable session state and filesystem worktrees.
func NewService(worktrees *Manager, store SessionStore) (*Service, error) {
	if worktrees == nil || store == nil {
		return nil, errors.New("code session worktree manager and store are required")
	}
	return &Service{worktrees: worktrees, store: store}, nil
}

// Create creates the worktree before publishing the durable ready session.
func (s *Service) Create(ctx context.Context, request CreateSessionRequest) (Session, error) {
	request.AuthorID = strings.TrimSpace(request.AuthorID)
	request.Provider = strings.TrimSpace(request.Provider)
	if request.RepositoryID <= 0 || request.AuthorID == "" || request.Provider == "" {
		return Session{}, errors.New("repository, author, and provider are required")
	}
	enabled, err := s.store.RepositoryCodingEnabled(ctx, request.RepositoryID)
	if err != nil {
		return Session{}, err
	}
	if !enabled {
		return Session{}, errors.New("coding is not enabled for this repository")
	}
	id, err := newSessionID()
	if err != nil {
		return Session{}, err
	}
	workspace, err := s.worktrees.Create(ctx, CreateRequest{
		ID: id, RepositoryID: request.RepositoryID, Baseline: request.Baseline,
	})
	if err != nil {
		return Session{}, err
	}
	now := time.Now().UTC()
	session := Session{
		ID: id, RepositoryID: workspace.RepositoryID, Repository: workspace.Repository,
		AuthorID: request.AuthorID, Provider: request.Provider,
		Model: strings.TrimSpace(request.Model), Effort: strings.TrimSpace(request.Effort),
		Baseline: workspace.Baseline, Branch: workspace.Branch,
		State: StateReady, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.CreateCodeSession(ctx, session); err != nil {
		_ = s.worktrees.Remove(context.Background(), id)
		return Session{}, err
	}
	_, _ = s.store.AppendCodeAction(ctx, Action{
		SessionID: id, Kind: "workspace_created", Status: "complete",
		Summary: "Isolated code worktree created", CreatedAt: now,
	})
	return session, nil
}

// Session returns an owner-scoped session.
func (s *Service) Session(ctx context.Context, id, authorID string) (Session, error) {
	session, err := s.store.CodeSession(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if session.AuthorID != strings.TrimSpace(authorID) {
		return Session{}, errors.New("code session was not found")
	}
	return session, nil
}

func (s *Service) List(ctx context.Context, authorID string) ([]Session, error) {
	return s.store.ListCodeSessions(ctx, strings.TrimSpace(authorID))
}

// RepositoryEnabled reports the explicit write-workspace policy. Callers must
// still enforce repository read access and the developer permission.
func (s *Service) RepositoryEnabled(ctx context.Context, repositoryID int64) (bool, error) {
	return s.store.RepositoryCodingEnabled(ctx, repositoryID)
}

// RecoverActive marks provider-bound states interrupted during application
// startup. Provider processes do not survive a RepoKarta restart, while their
// owned worktrees and durable transcripts do.
func (s *Service) RecoverActive(ctx context.Context) (int64, error) {
	return s.store.RecoverActiveCodeSessions(ctx, time.Now().UTC())
}

func (s *Service) Actions(ctx context.Context, id, authorID string) ([]Action, error) {
	if _, err := s.Session(ctx, id, authorID); err != nil {
		return nil, err
	}
	return s.store.CodeActions(ctx, id, 500)
}

func (s *Service) Approvals(ctx context.Context, id, authorID string) ([]Approval, error) {
	if _, err := s.Session(ctx, id, authorID); err != nil {
		return nil, err
	}
	return s.store.CodeApprovals(ctx, id)
}

func (s *Service) Diff(ctx context.Context, id, authorID string, contextLines int) (Diff, error) {
	if _, err := s.Session(ctx, id, authorID); err != nil {
		return Diff{}, err
	}
	return s.worktrees.Diff(ctx, id, contextLines)
}

func (s *Service) ReadFile(ctx context.Context, id, authorID, path string) (File, error) {
	if _, err := s.Session(ctx, id, authorID); err != nil {
		return File{}, err
	}
	return s.worktrees.ReadFile(ctx, id, path, MaximumPreviewBytes)
}

func (s *Service) WorkspacePath(ctx context.Context, id, authorID string) (string, error) {
	if _, err := s.Session(ctx, id, authorID); err != nil {
		return "", err
	}
	workspace, err := s.worktrees.Workspace(id)
	if err != nil {
		return "", err
	}
	return workspace.CheckoutPath, nil
}

func (s *Service) AttachConversation(
	ctx context.Context,
	id, authorID, conversationID string,
) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.Session(ctx, id, authorID)
	if err != nil {
		return Session{}, err
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return Session{}, errors.New("conversation ID is required")
	}
	if session.ConversationID != "" && session.ConversationID != conversationID {
		return Session{}, errors.New("code session already has a different conversation")
	}
	session.ConversationID = conversationID
	session.Version++
	session.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateCodeSession(ctx, session); err != nil {
		return Session{}, err
	}
	return session, nil
}

func (s *Service) SetState(
	ctx context.Context,
	id, authorID, state, detail string,
) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.Session(ctx, id, authorID)
	if err != nil {
		return Session{}, err
	}
	if !validState(state) {
		return Session{}, errors.New("invalid code session state")
	}
	session.State = state
	session.Error = strings.TrimSpace(detail)
	session.Version++
	session.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateCodeSession(ctx, session); err != nil {
		return Session{}, err
	}
	return session, nil
}

func (s *Service) RecordAction(ctx context.Context, action Action) (Action, error) {
	action.Summary = boundText(action.Summary, 2048)
	action.Command = boundText(action.Command, 4096)
	action.Output = boundText(action.Output, 64<<10)
	if action.CreatedAt.IsZero() {
		action.CreatedAt = time.Now().UTC()
	}
	return s.store.AppendCodeAction(ctx, action)
}

func (s *Service) RecordApproval(ctx context.Context, approval Approval) error {
	approval.Reason = boundText(approval.Reason, 2048)
	approval.Command = boundText(approval.Command, 4096)
	approval.Directory = boundText(approval.Directory, 1024)
	if approval.RequestedAt.IsZero() {
		approval.RequestedAt = time.Now().UTC()
	}
	approval.Status = "pending"
	return s.store.CreateCodeApproval(ctx, approval)
}

func (s *Service) ResolveApproval(
	ctx context.Context,
	id, authorID, approvalID, decision string,
) (Approval, error) {
	if _, err := s.Session(ctx, id, authorID); err != nil {
		return Approval{}, err
	}
	approval, err := s.store.CodeApproval(ctx, approvalID)
	if err != nil {
		return Approval{}, err
	}
	if approval.SessionID != id || approval.Status != "pending" {
		return Approval{}, errors.New("code approval is not pending for this session")
	}
	switch decision {
	case "accept", "decline", "cancel":
	default:
		return Approval{}, errors.New("invalid code approval decision")
	}
	now := time.Now().UTC()
	if err := s.store.ResolveCodeApproval(ctx, approvalID, "resolved", decision, now); err != nil {
		return Approval{}, err
	}
	approval.Status = "resolved"
	approval.Decision = decision
	approval.ResolvedAt = now
	return approval, nil
}

func (s *Service) DiscardFile(
	ctx context.Context,
	id, authorID, path, expectedDiffVersion string,
) (Diff, error) {
	if _, err := s.Session(ctx, id, authorID); err != nil {
		return Diff{}, err
	}
	diff, err := s.worktrees.DiscardFile(ctx, id, path, expectedDiffVersion)
	if err == nil {
		_, _ = s.RecordAction(ctx, Action{
			SessionID: id, Kind: "file_discarded", Status: "complete",
			Summary: path,
		})
	}
	return diff, err
}

func (s *Service) Finish(
	ctx context.Context,
	id, authorID, message, expectedDiffVersion string,
) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.Session(ctx, id, authorID)
	if err != nil {
		return Session{}, err
	}
	if session.State == StateRunning || session.State == StateAwaitingApproval {
		return Session{}, errors.New("cannot finish while a code turn is active")
	}
	result, err := s.worktrees.Finish(ctx, id, message, expectedDiffVersion)
	if err != nil {
		return Session{}, err
	}
	now := time.Now().UTC()
	session.State = StateFinished
	session.FinishedCommit = result.Commit
	session.FinishedAt = now
	session.UpdatedAt = now
	session.Version++
	if err := s.store.UpdateCodeSession(ctx, session); err != nil {
		return Session{}, err
	}
	_, _ = s.RecordAction(ctx, Action{
		SessionID: id, Kind: "finished", Status: "complete",
		Summary: result.Commit, CreatedAt: now,
	})
	return session, nil
}

func (s *Service) Discard(ctx context.Context, id, authorID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.Session(ctx, id, authorID)
	if err != nil {
		return err
	}
	if session.State == StateRunning || session.State == StateAwaitingApproval {
		return errors.New("cannot discard while a code turn is active")
	}
	if err := s.worktrees.Remove(ctx, id); err != nil {
		return err
	}
	return s.store.DeleteCodeSession(ctx, id)
}

func newSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "code-" + hex.EncodeToString(value[:]), nil
}

func validState(state string) bool {
	switch state {
	case StateCreating, StateReady, StateRunning, StateAwaitingApproval,
		StateInterrupted, StateFinishing, StateFinished, StateDiscarding,
		StateDiscarded, StateFailed:
		return true
	default:
		return false
	}
}

func boundText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
