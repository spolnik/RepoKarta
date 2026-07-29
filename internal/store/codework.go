package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/codework"
)

// RepositoryCodingEnabled reports the explicit operator-controlled write flag.
func (s *Store) RepositoryCodingEnabled(ctx context.Context, repositoryID int64) (bool, error) {
	var enabled bool
	err := s.db.QueryRowContext(ctx, `
SELECT code_enabled FROM repository_access WHERE repository_id = ?`,
		repositoryID).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("repository %d is not indexed", repositoryID)
	}
	return enabled, err
}

func (s *Store) CreateCodeSession(ctx context.Context, session codework.Session) error {
	if strings.TrimSpace(session.ID) == "" || session.RepositoryID <= 0 ||
		strings.TrimSpace(session.AuthorID) == "" {
		return errors.New("invalid code session")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO code_sessions (
    id, repository_id, repository_name, conversation_id, author_id,
    provider, model, effort, baseline, branch, state, version, error,
    finished_commit, created_at, updated_at, finished_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID, session.RepositoryID, session.Repository, session.ConversationID,
		session.AuthorID, session.Provider, session.Model, session.Effort,
		session.Baseline, session.Branch, session.State, session.Version,
		session.Error, session.FinishedCommit, formatTime(session.CreatedAt),
		formatTime(session.UpdatedAt), formatTime(session.FinishedAt),
	)
	if err != nil {
		return fmt.Errorf("create code session: %w", err)
	}
	return nil
}

func (s *Store) CodeSession(ctx context.Context, id string) (codework.Session, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, repository_id, repository_name, conversation_id, author_id,
       provider, model, effort, baseline, branch, state, version, error,
       finished_commit, created_at, updated_at, finished_at
FROM code_sessions WHERE id = ?`, id)
	session, err := scanCodeSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return codework.Session{}, errors.New("code session was not found")
	}
	return session, err
}

func (s *Store) ListCodeSessions(ctx context.Context, authorID string) ([]codework.Session, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, repository_id, repository_name, conversation_id, author_id,
       provider, model, effort, baseline, branch, state, version, error,
       finished_commit, created_at, updated_at, finished_at
FROM code_sessions
WHERE author_id = ?
ORDER BY updated_at DESC, id`, authorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []codework.Session
	for rows.Next() {
		session, err := scanCodeSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (s *Store) UpdateCodeSession(ctx context.Context, session codework.Session) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE code_sessions
SET conversation_id = ?, provider = ?, model = ?, effort = ?, state = ?,
    version = ?, error = ?, finished_commit = ?, updated_at = ?, finished_at = ?
WHERE id = ? AND author_id = ?`,
		session.ConversationID, session.Provider, session.Model, session.Effort,
		session.State, session.Version, session.Error, session.FinishedCommit,
		formatTime(session.UpdatedAt), formatTime(session.FinishedAt),
		session.ID, session.AuthorID,
	)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return errors.New("code session was not found")
	}
	return nil
}

func (s *Store) RecoverActiveCodeSessions(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE code_sessions
SET state = 'interrupted', error = 'RepoKarta restarted while the provider turn was active',
    version = version + 1, updated_at = ?
WHERE state IN ('running', 'awaiting_approval')`, formatTime(now))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) DeleteCodeSession(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM code_sessions WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return errors.New("code session was not found")
	}
	return nil
}

func (s *Store) AppendCodeAction(ctx context.Context, action codework.Action) (codework.Action, error) {
	if action.CreatedAt.IsZero() {
		action.CreatedAt = time.Now().UTC()
	}
	var exitCode any
	if action.ExitCode != nil {
		exitCode = *action.ExitCode
	}
	err := s.db.QueryRowContext(ctx, `
INSERT INTO code_actions (
    session_id, kind, status, summary, command, output,
    exit_code, approval_id, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id`,
		action.SessionID, action.Kind, action.Status, action.Summary,
		action.Command, action.Output, exitCode, action.ApprovalID,
		formatTime(action.CreatedAt),
	).Scan(&action.ID)
	if err != nil {
		return codework.Action{}, fmt.Errorf("append code action: %w", err)
	}
	return action, nil
}

func (s *Store) CodeActions(ctx context.Context, sessionID string, limit int) ([]codework.Action, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, session_id, kind, status, summary, command, output,
       exit_code, approval_id, created_at
FROM code_actions
WHERE session_id = ?
ORDER BY id DESC
LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reverse []codework.Action
	for rows.Next() {
		var (
			action   codework.Action
			exitCode sql.NullInt64
			created  string
		)
		if err := rows.Scan(
			&action.ID, &action.SessionID, &action.Kind, &action.Status,
			&action.Summary, &action.Command, &action.Output, &exitCode,
			&action.ApprovalID, &created,
		); err != nil {
			return nil, err
		}
		if exitCode.Valid {
			value := int(exitCode.Int64)
			action.ExitCode = &value
		}
		action.CreatedAt = parseTime(created)
		reverse = append(reverse, action)
	}
	actions := make([]codework.Action, len(reverse))
	for index := range reverse {
		actions[len(reverse)-1-index] = reverse[index]
	}
	return actions, rows.Err()
}

func (s *Store) CreateCodeApproval(ctx context.Context, approval codework.Approval) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO code_approvals (
    id, session_id, kind, reason, command, directory, status, decision,
    requested_at, resolved_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		approval.ID, approval.SessionID, approval.Kind, approval.Reason,
		approval.Command, approval.Directory, approval.Status, approval.Decision,
		formatTime(approval.RequestedAt), formatTime(approval.ResolvedAt),
	)
	return err
}

func (s *Store) CodeApproval(ctx context.Context, id string) (codework.Approval, error) {
	var (
		approval  codework.Approval
		requested string
		resolved  string
	)
	err := s.db.QueryRowContext(ctx, `
SELECT id, session_id, kind, reason, command, directory, status, decision,
       requested_at, resolved_at
FROM code_approvals WHERE id = ?`, id).Scan(
		&approval.ID, &approval.SessionID, &approval.Kind, &approval.Reason,
		&approval.Command, &approval.Directory, &approval.Status, &approval.Decision,
		&requested, &resolved,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return codework.Approval{}, errors.New("code approval was not found")
	}
	approval.RequestedAt = parseTime(requested)
	approval.ResolvedAt = parseTime(resolved)
	return approval, err
}

func (s *Store) CodeApprovals(ctx context.Context, sessionID string) ([]codework.Approval, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, session_id, kind, reason, command, directory, status, decision,
       requested_at, resolved_at
FROM code_approvals
WHERE session_id = ?
ORDER BY requested_at, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var approvals []codework.Approval
	for rows.Next() {
		var approval codework.Approval
		var requested, resolved string
		if err := rows.Scan(
			&approval.ID, &approval.SessionID, &approval.Kind, &approval.Reason,
			&approval.Command, &approval.Directory, &approval.Status, &approval.Decision,
			&requested, &resolved,
		); err != nil {
			return nil, err
		}
		approval.RequestedAt = parseTime(requested)
		approval.ResolvedAt = parseTime(resolved)
		approvals = append(approvals, approval)
	}
	return approvals, rows.Err()
}

func (s *Store) ResolveCodeApproval(
	ctx context.Context,
	id, status, decision string,
	resolvedAt time.Time,
) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE code_approvals
SET status = ?, decision = ?, resolved_at = ?
WHERE id = ? AND status = 'pending'`,
		status, decision, formatTime(resolvedAt), id,
	)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return errors.New("code approval is no longer pending")
	}
	return nil
}

type codeSessionScanner interface {
	Scan(...any) error
}

func scanCodeSession(scanner codeSessionScanner) (codework.Session, error) {
	var (
		session  codework.Session
		created  string
		updated  string
		finished string
	)
	err := scanner.Scan(
		&session.ID, &session.RepositoryID, &session.Repository,
		&session.ConversationID, &session.AuthorID, &session.Provider,
		&session.Model, &session.Effort, &session.Baseline, &session.Branch,
		&session.State, &session.Version, &session.Error,
		&session.FinishedCommit, &created, &updated, &finished,
	)
	session.CreatedAt = parseTime(created)
	session.UpdatedAt = parseTime(updated)
	session.FinishedAt = parseTime(finished)
	return session, err
}
