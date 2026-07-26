package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/acquisition"
)

const acquisitionColumns = `
id, provider, provider_repository_id, canonical_id, name, namespace, remote_url, web_url, checkout_path,
default_branch, credential_ref, inclusion_policy, visibility, archived, forked, owned, state,
last_error, head_commit, failure_count, created_at, discovered_at, synced_at,
next_sync_at, updated_at`

// ListAcquisitions returns every administrator-approved source in display order.
func (s *Store) ListAcquisitions(ctx context.Context) ([]acquisition.Repository, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT `+acquisitionColumns+`
FROM repository_acquisitions
ORDER BY provider, canonical_id COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repositories []acquisition.Repository
	for rows.Next() {
		repository, err := scanAcquisition(rows)
		if err != nil {
			return nil, err
		}
		repositories = append(repositories, repository)
	}
	return repositories, rows.Err()
}

// AcquisitionByID returns one approved source.
func (s *Store) AcquisitionByID(ctx context.Context, id int64) (acquisition.Repository, error) {
	if id <= 0 {
		return acquisition.Repository{}, errors.New("repository acquisition ID is required")
	}
	repository, err := scanAcquisition(s.db.QueryRowContext(ctx, `
SELECT `+acquisitionColumns+`
FROM repository_acquisitions
WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return acquisition.Repository{}, fmt.Errorf("repository acquisition %d does not exist", id)
	}
	return repository, err
}

// UpsertAcquisition creates or updates one source registry record.
func (s *Store) UpsertAcquisition(ctx context.Context, repository acquisition.Repository) (acquisition.Repository, error) {
	repository.Provider = strings.ToLower(strings.TrimSpace(repository.Provider))
	repository.CanonicalID = strings.TrimSpace(repository.CanonicalID)
	repository.Name = strings.TrimSpace(repository.Name)
	repository.CheckoutPath = strings.TrimSpace(repository.CheckoutPath)
	repository.CredentialRef = strings.TrimSpace(repository.CredentialRef)
	repository.ProviderRepositoryID = strings.TrimSpace(repository.ProviderRepositoryID)
	repository.InclusionPolicy = strings.TrimSpace(repository.InclusionPolicy)
	if repository.InclusionPolicy == "" {
		repository.InclusionPolicy = "approved"
	}
	if repository.Provider == "" || repository.CanonicalID == "" || repository.Name == "" ||
		repository.CheckoutPath == "" || repository.State == "" {
		return acquisition.Repository{}, errors.New("provider, canonical identity, name, checkout path, and state are required")
	}
	now := time.Now().UTC()
	if repository.CreatedAt.IsZero() {
		repository.CreatedAt = now
	}
	if repository.DiscoveredAt.IsZero() {
		repository.DiscoveredAt = repository.CreatedAt
	}
	if repository.UpdatedAt.IsZero() {
		repository.UpdatedAt = now
	}
	if repository.ID == 0 {
		result, err := s.db.ExecContext(ctx, `
INSERT INTO repository_acquisitions (
    provider, provider_repository_id, canonical_id, name, namespace, remote_url, web_url, checkout_path,
    default_branch, credential_ref, inclusion_policy, visibility, archived, forked, owned, state,
    last_error, head_commit, failure_count, created_at, discovered_at, synced_at,
    next_sync_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			acquisitionArguments(repository)...)
		if err != nil {
			return acquisition.Repository{}, fmt.Errorf("register repository acquisition %q: %w", repository.CanonicalID, err)
		}
		repository.ID, err = result.LastInsertId()
		if err != nil {
			return acquisition.Repository{}, err
		}
	} else {
		result, err := s.db.ExecContext(ctx, `
UPDATE repository_acquisitions SET
    provider = ?, provider_repository_id = ?, canonical_id = ?, name = ?, namespace = ?, remote_url = ?,
    web_url = ?, checkout_path = ?, default_branch = ?, credential_ref = ?,
    inclusion_policy = ?, visibility = ?, archived = ?, forked = ?, owned = ?, state = ?,
    last_error = ?, head_commit = ?, failure_count = ?, created_at = ?,
    discovered_at = ?, synced_at = ?, next_sync_at = ?, updated_at = ?
WHERE id = ?`,
			append(acquisitionArguments(repository), repository.ID)...)
		if err != nil {
			return acquisition.Repository{}, fmt.Errorf("update repository acquisition %q: %w", repository.CanonicalID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return acquisition.Repository{}, err
		}
		if affected != 1 {
			return acquisition.Repository{}, fmt.Errorf("repository acquisition %d does not exist", repository.ID)
		}
	}
	return s.AcquisitionByID(ctx, repository.ID)
}

func acquisitionArguments(repository acquisition.Repository) []any {
	return []any{
		repository.Provider,
		strings.TrimSpace(repository.ProviderRepositoryID),
		repository.CanonicalID,
		repository.Name,
		strings.TrimSpace(repository.Namespace),
		strings.TrimSpace(repository.RemoteURL),
		strings.TrimSpace(repository.WebURL),
		repository.CheckoutPath,
		strings.TrimSpace(repository.DefaultBranch),
		repository.CredentialRef,
		strings.TrimSpace(repository.InclusionPolicy),
		strings.TrimSpace(repository.Visibility),
		repository.Archived,
		repository.Forked,
		repository.Owned,
		repository.State,
		strings.TrimSpace(repository.LastError),
		strings.TrimSpace(repository.HeadCommit),
		repository.FailureCount,
		formatTime(repository.CreatedAt),
		formatTime(repository.DiscoveredAt),
		formatTime(repository.SyncedAt),
		formatTime(repository.NextSyncAt),
		formatTime(repository.UpdatedAt),
	}
}

// DeleteAcquisition removes only the source registry entry. Filesystem removal
// is owned by acquisition.Service, which proves the target before moving it.
func (s *Store) DeleteAcquisition(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM repository_acquisitions WHERE id = ?`, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("repository acquisition %d does not exist", id)
	}
	return nil
}

// RecordAcquisitionEvent appends source-free audit metadata. There is no
// foreign key so removal events and historical provenance survive unregistering.
func (s *Store) RecordAcquisitionEvent(ctx context.Context, event acquisition.Event) error {
	if strings.TrimSpace(event.Action) == "" || strings.TrimSpace(event.Outcome) == "" {
		return errors.New("acquisition event action and outcome are required")
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO repository_acquisition_events (
    repository_id, canonical_id, action, outcome, revision, detail, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		event.RepositoryID,
		strings.TrimSpace(event.CanonicalID),
		strings.TrimSpace(event.Action),
		strings.TrimSpace(event.Outcome),
		strings.TrimSpace(event.Revision),
		strings.TrimSpace(event.Detail),
		formatTime(event.CreatedAt),
	)
	return err
}

type acquisitionScanner interface {
	Scan(...any) error
}

func scanAcquisition(scanner acquisitionScanner) (acquisition.Repository, error) {
	var repository acquisition.Repository
	var archived, forked, owned bool
	var createdAt, discoveredAt, syncedAt, nextSyncAt, updatedAt string
	err := scanner.Scan(
		&repository.ID,
		&repository.Provider,
		&repository.ProviderRepositoryID,
		&repository.CanonicalID,
		&repository.Name,
		&repository.Namespace,
		&repository.RemoteURL,
		&repository.WebURL,
		&repository.CheckoutPath,
		&repository.DefaultBranch,
		&repository.CredentialRef,
		&repository.InclusionPolicy,
		&repository.Visibility,
		&archived,
		&forked,
		&owned,
		&repository.State,
		&repository.LastError,
		&repository.HeadCommit,
		&repository.FailureCount,
		&createdAt,
		&discoveredAt,
		&syncedAt,
		&nextSyncAt,
		&updatedAt,
	)
	if err != nil {
		return acquisition.Repository{}, err
	}
	repository.Archived = archived
	repository.Forked = forked
	repository.Owned = owned
	repository.CreatedAt = parseTime(createdAt)
	repository.DiscoveredAt = parseTime(discoveredAt)
	repository.SyncedAt = parseTime(syncedAt)
	repository.NextSyncAt = parseTime(nextSyncAt)
	repository.UpdatedAt = parseTime(updatedAt)
	return repository, nil
}
