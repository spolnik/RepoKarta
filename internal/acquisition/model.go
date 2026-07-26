package acquisition

import "time"

const (
	ProviderLocal  = "local"
	ProviderGitHub = "github"
	ProviderGitLab = "gitlab"

	StateAcquiring = "acquiring"
	StateReady     = "ready"
	StateSyncing   = "syncing"
	StateError     = "error"
)

// Candidate is one repository returned by a bounded discovery preview.
type Candidate struct {
	Provider             string   `json:"provider"`
	ProviderRepositoryID string   `json:"provider_repository_id,omitempty"`
	CanonicalID          string   `json:"canonical_id"`
	Name                 string   `json:"name"`
	Namespace            string   `json:"namespace"`
	RemoteURL            string   `json:"remote_url"`
	WebURL               string   `json:"web_url"`
	LocalPath            string   `json:"local_path"`
	DefaultBranch        string   `json:"default_branch"`
	Visibility           string   `json:"visibility"`
	Topics               []string `json:"topics,omitempty"`
	Archived             bool     `json:"archived"`
	Forked               bool     `json:"forked"`
	AlreadyManaged       bool     `json:"already_managed"`
	Excluded             bool     `json:"excluded"`
	Exclusion            string   `json:"exclusion,omitempty"`
	InclusionPolicy      string   `json:"inclusion_policy"`
}

// DiscoverRequest describes one local-root or hosted-namespace preview.
type DiscoverRequest struct {
	Provider        string
	Location        string
	CredentialRef   string
	IncludeArchived bool
	IncludeForks    bool
	IncludePrivate  bool
	Team            string
	Topics          []string
	Allow           []string
	Deny            []string
}

// Repository is durable acquisition provenance for an approved repository.
// CredentialRef is the name of an environment variable or credential helper
// reference; secret values are never persisted.
type Repository struct {
	ID                   int64     `json:"id"`
	Provider             string    `json:"provider"`
	ProviderRepositoryID string    `json:"provider_repository_id,omitempty"`
	CanonicalID          string    `json:"canonical_id"`
	Name                 string    `json:"name"`
	Namespace            string    `json:"namespace"`
	RemoteURL            string    `json:"remote_url"`
	WebURL               string    `json:"web_url"`
	CheckoutPath         string    `json:"checkout_path"`
	DefaultBranch        string    `json:"default_branch"`
	CredentialRef        string    `json:"credential_ref,omitempty"`
	InclusionPolicy      string    `json:"inclusion_policy"`
	Visibility           string    `json:"visibility"`
	Archived             bool      `json:"archived"`
	Forked               bool      `json:"forked"`
	Owned                bool      `json:"owned"`
	State                string    `json:"state"`
	LastError            string    `json:"last_error,omitempty"`
	HeadCommit           string    `json:"head_commit,omitempty"`
	FailureCount         int       `json:"failure_count"`
	CreatedAt            time.Time `json:"created_at"`
	DiscoveredAt         time.Time `json:"discovered_at"`
	SyncedAt             time.Time `json:"synced_at,omitempty"`
	NextSyncAt           time.Time `json:"next_sync_at,omitempty"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// Event is a source-free acquisition audit record.
type Event struct {
	ID           int64
	RepositoryID int64
	CanonicalID  string
	Action       string
	Outcome      string
	Revision     string
	Detail       string
	CreatedAt    time.Time
}
