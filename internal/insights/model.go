// Package insights imports and exposes commit-aware code quality observations.
// It never executes repository build scripts, tests, scanners, or linters.
package insights

import (
	"context"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

const (
	KindMetric  = "metric"
	KindFinding = "finding"

	StateMeasured       = "measured"
	StateDerived        = "derived"
	StateSkipped        = "skipped"
	StateParseError     = "parse_error"
	StateUnresolvedPath = "unresolved_path"

	StatusCurrent     = "current"
	StatusPartial     = "partial"
	StatusQuarantined = "quarantined"
	StatusStale       = "stale"
	StatusUnavailable = "unavailable"
	StatusRateLimited = "rate_limited"
)

// Run describes one immutable report, external poll, or deterministic
// RepoKarta derivation.
type Run struct {
	ID               string            `json:"id"`
	RepositoryID     int64             `json:"repository_id"`
	Repository       string            `json:"repository"`
	Revision         string            `json:"revision"`
	Branch           string            `json:"branch,omitempty"`
	Tool             string            `json:"tool"`
	ToolVersion      string            `json:"tool_version,omitempty"`
	SourceKind       string            `json:"source_kind"`
	SourceRef        string            `json:"source_ref,omitempty"`
	RulePack         string            `json:"rule_pack,omitempty"`
	Configuration    string            `json:"configuration,omitempty"`
	License          string            `json:"license,omitempty"`
	Status           string            `json:"status"`
	StatusMessage    string            `json:"status_message,omitempty"`
	Confidence       string            `json:"confidence"`
	ObservedAt       time.Time         `json:"observed_at"`
	IngestedAt       time.Time         `json:"ingested_at"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	ObservationCount int               `json:"observation_count"`
}

// Observation is one normalized metric or finding with optional exact source
// evidence. Nil Value distinguishes a missing measurement from numeric zero.
type Observation struct {
	ID           int64          `json:"id"`
	RunID        string         `json:"run_id"`
	RepositoryID int64          `json:"repository_id"`
	Repository   string         `json:"repository"`
	Revision     string         `json:"revision"`
	Branch       string         `json:"branch,omitempty"`
	Tool         string         `json:"tool"`
	ToolVersion  string         `json:"tool_version,omitempty"`
	Kind         string         `json:"kind"`
	Key          string         `json:"key"`
	Value        *float64       `json:"value,omitempty"`
	Unit         string         `json:"unit,omitempty"`
	Severity     string         `json:"severity,omitempty"`
	Message      string         `json:"message,omitempty"`
	Path         string         `json:"path,omitempty"`
	StartLine    int            `json:"start_line,omitempty"`
	EndLine      int            `json:"end_line,omitempty"`
	Language     string         `json:"language,omitempty"`
	Owner        string         `json:"owner,omitempty"`
	Fingerprint  string         `json:"fingerprint,omitempty"`
	Suppressed   bool           `json:"suppressed"`
	State        string         `json:"state"`
	Confidence   string         `json:"confidence"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	CodeFlows    any            `json:"code_flows,omitempty"`
	SourceURL    string         `json:"source_url,omitempty"`
	ObservedAt   time.Time      `json:"observed_at"`
}

// Filter bounds a deterministic observation query.
type Filter struct {
	RepositoryID       int64
	RepositoryIDs      []int64
	Revision           string
	Branch             string
	Directory          string
	File               string
	Language           string
	Tool               string
	Rule               string
	Severity           string
	Owner              string
	Kind               string
	Since              time.Time
	Until              time.Time
	IncludeQuarantined bool
	Limit              int
}

type Facets struct {
	Repositories map[string]int `json:"repositories"`
	Branches     map[string]int `json:"branches"`
	Languages    map[string]int `json:"languages"`
	Tools        map[string]int `json:"tools"`
	Rules        map[string]int `json:"rules"`
	Severities   map[string]int `json:"severities"`
	Owners       map[string]int `json:"owners"`
	States       map[string]int `json:"states"`
}

// QueryResponse separates the latest value/finding for each stable identity
// from bounded historical observations.
type QueryResponse struct {
	Current     []Observation `json:"current"`
	History     []Observation `json:"history"`
	Runs        []Run         `json:"runs"`
	Facets      Facets        `json:"facets"`
	Truncated   bool          `json:"truncated"`
	Warnings    []string      `json:"warnings,omitempty"`
	GeneratedAt time.Time     `json:"generated_at"`
}

type Comparison struct {
	RepositoryID int64         `json:"repository_id"`
	FromRevision string        `json:"from_revision"`
	ToRevision   string        `json:"to_revision"`
	MetricDeltas []MetricDelta `json:"metric_deltas"`
	Introduced   []Observation `json:"introduced_findings"`
	Resolved     []Observation `json:"resolved_findings"`
	Warnings     []string      `json:"warnings,omitempty"`
}

type MetricDelta struct {
	Key       string   `json:"key"`
	Path      string   `json:"path,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	FromValue *float64 `json:"from_value,omitempty"`
	ToValue   *float64 `json:"to_value,omitempty"`
	Delta     *float64 `json:"delta,omitempty"`
}

// Threshold is advisory. Evaluation never claims RepoKarta enforced a CI gate.
type Threshold struct {
	ID           int64     `json:"id"`
	RepositoryID int64     `json:"repository_id,omitempty"`
	Key          string    `json:"key"`
	Operator     string    `json:"operator"`
	Value        float64   `json:"value"`
	Severity     string    `json:"severity"`
	Enabled      bool      `json:"enabled"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type ThresholdEvaluation struct {
	Threshold Threshold   `json:"threshold"`
	Observed  Observation `json:"observed"`
	Violated  bool        `json:"violated"`
	Advisory  bool        `json:"advisory"`
}

// SonarConnection stores no credential value. TokenEnv names the environment
// variable read at poll time, which supports rotation without database writes.
type SonarConnection struct {
	ID                  int64     `json:"id"`
	RepositoryID        int64     `json:"repository_id"`
	Repository          string    `json:"repository,omitempty"`
	BaseURL             string    `json:"base_url"`
	ProjectKey          string    `json:"project_key"`
	TokenEnv            string    `json:"token_env"`
	PollIntervalMinutes int       `json:"poll_interval_minutes"`
	RetentionRuns       int       `json:"retention_runs"`
	Enabled             bool      `json:"enabled"`
	State               string    `json:"state"`
	StatusMessage       string    `json:"status_message,omitempty"`
	LastPolledAt        time.Time `json:"last_polled_at,omitempty"`
	NextPollAt          time.Time `json:"next_poll_at,omitempty"`
	FailureCount        int       `json:"failure_count"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type ImportRequest struct {
	RepositoryID  int64
	Revision      string
	Branch        string
	Format        string
	Tool          string
	ToolVersion   string
	SourceKind    string
	SourceRef     string
	RulePack      string
	Configuration string
	License       string
	Owner         string
	PathPrefix    string
	ObservedAt    time.Time
	Content       []byte
}

// RepositoryStore is implemented by the application metadata store.
type RepositoryStore interface {
	ListRepositories(context.Context) ([]catalog.Repository, error)
	RepositoryByID(context.Context, int64) (catalog.Repository, error)
	SaveInsightRun(context.Context, Run, []Observation) error
	ListInsightRuns(context.Context, Filter) ([]Run, error)
	ListInsightObservations(context.Context, Filter) ([]Observation, error)
	DeleteOldInsightRuns(context.Context, int64, string, int) error
	ListInsightThresholds(context.Context, int64) ([]Threshold, error)
	UpsertInsightThreshold(context.Context, Threshold) (Threshold, error)
	UpsertSonarConnection(context.Context, SonarConnection) (SonarConnection, error)
	ListSonarConnections(context.Context, bool) ([]SonarConnection, error)
	UpdateSonarConnectionState(context.Context, SonarConnection) error
}
