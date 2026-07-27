package contextscope

import (
	"errors"
	"time"
)

const (
	CategoryTeam         = "team"
	CategoryProduct      = "product"
	CategoryServiceFleet = "service_fleet"
	CategoryRelease      = "release"
	CategoryPersonalTask = "personal_task"

	VisibilityPersonal = "personal"
	VisibilityShared   = "shared"

	DefaultNone          = "none"
	DefaultPersonal      = "personal"
	DefaultAdministrator = "administrator"

	SourceExplicit             = "explicit"
	SourceNamed                = "named"
	SourcePersonalDefault      = "personal_default"
	SourceAdministratorDefault = "administrator_default"
)

var (
	ErrNamedContextNotFound  = errors.New("named context not found")
	ErrNamedContextForbidden = errors.New("named context is not editable by the current viewer")
	ErrNamedContextConflict  = errors.New("a named context with this title already exists")
)

// NamedContextRecord is the durable definition. Selectors are exact,
// repository-level identities pinned to the indexed revisions that were
// current when the definition was saved.
type NamedContextRecord struct {
	ID           string     `json:"id"`
	Title        string     `json:"title"`
	Description  string     `json:"description,omitempty"`
	Category     string     `json:"category"`
	Visibility   string     `json:"visibility"`
	DefaultScope string     `json:"default_scope"`
	OwnerID      string     `json:"owner_id"`
	Managed      bool       `json:"managed"`
	Selectors    []Selector `json:"selectors"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// NamedContextInput is accepted by the JSON API. Ownership and management
// fields are always derived from the authenticated viewer.
type NamedContextInput struct {
	Title        string     `json:"title"`
	Description  string     `json:"description,omitempty"`
	Category     string     `json:"category"`
	Visibility   string     `json:"visibility"`
	DefaultScope string     `json:"default_scope"`
	Selectors    []Selector `json:"selectors"`
}

// NamedContext is a permission-checked API/MCP view. Contexts contain
// canonical URLs and the exact currently effective revisions.
type NamedContext struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Description  string    `json:"description,omitempty"`
	Category     string    `json:"category"`
	Visibility   string    `json:"visibility"`
	DefaultScope string    `json:"default_scope"`
	OwnerID      string    `json:"owner_id"`
	Managed      bool      `json:"managed"`
	Editable     bool      `json:"editable"`
	State        string    `json:"state"`
	Issues       []Issue   `json:"issues,omitempty"`
	URL          string    `json:"url"`
	Contexts     []Context `json:"contexts"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type NamedContextList struct {
	NamedContexts []NamedContext `json:"named_contexts"`
}

// EffectiveRequest expands explicit selectors, selected named contexts, and
// personal/administrator defaults into one fail-closed context set.
// UseDefaults defaults to true when omitted.
type EffectiveRequest struct {
	Contexts        []Selector `json:"contexts,omitempty"`
	NamedContextIDs []string   `json:"named_context_ids,omitempty"`
	UseDefaults     *bool      `json:"use_default_contexts,omitempty"`
}

type EffectiveResponse struct {
	Contexts      []Context      `json:"contexts"`
	NamedContexts []NamedContext `json:"named_contexts,omitempty"`
}
