package codeintel

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/spolnik/RepoKarta/internal/access"
	"github.com/spolnik/RepoKarta/internal/contextscope"
)

// NamedContextStore persists definitions independently from source indexes.
type NamedContextStore interface {
	ListNamedContextRecords(context.Context) ([]contextscope.NamedContextRecord, error)
	GetNamedContextRecord(context.Context, string) (contextscope.NamedContextRecord, error)
	CreateNamedContextRecord(context.Context, contextscope.NamedContextRecord) (contextscope.NamedContextRecord, error)
	UpdateNamedContextRecord(context.Context, string, contextscope.NamedContextRecord) (contextscope.NamedContextRecord, error)
	DeleteNamedContextRecord(context.Context, string) error
}

// UseNamedContexts enables durable reusable contexts and personal/admin
// defaults.
func (s *Service) UseNamedContexts(store NamedContextStore) *Service {
	s.namedContexts = store
	return s
}

func (s *Service) ListNamedContexts(ctx context.Context) (contextscope.NamedContextList, error) {
	output := contextscope.NamedContextList{NamedContexts: []contextscope.NamedContext{}}
	if s.namedContexts == nil {
		return output, nil
	}
	records, err := s.namedContexts.ListNamedContextRecords(ctx)
	if err != nil {
		return output, err
	}
	for _, record := range records {
		output.NamedContexts = append(output.NamedContexts, s.namedContextView(ctx, record))
	}
	return output, nil
}

func (s *Service) GetNamedContext(ctx context.Context, id string) (contextscope.NamedContext, error) {
	if s.namedContexts == nil {
		return contextscope.NamedContext{}, contextscope.ErrNamedContextNotFound
	}
	record, err := s.namedContexts.GetNamedContextRecord(ctx, strings.TrimSpace(id))
	if err != nil {
		return contextscope.NamedContext{}, err
	}
	return s.namedContextView(ctx, record), nil
}

func (s *Service) CreateNamedContext(
	ctx context.Context,
	input contextscope.NamedContextInput,
) (contextscope.NamedContext, error) {
	if s.namedContexts == nil {
		return contextscope.NamedContext{}, errors.New("named contexts are unavailable")
	}
	record, err := s.validNamedContextRecord(ctx, input)
	if err != nil {
		return contextscope.NamedContext{}, err
	}
	record, err = s.namedContexts.CreateNamedContextRecord(ctx, record)
	if err != nil {
		return contextscope.NamedContext{}, err
	}
	return s.namedContextView(ctx, record), nil
}

func (s *Service) UpdateNamedContext(
	ctx context.Context,
	id string,
	input contextscope.NamedContextInput,
) (contextscope.NamedContext, error) {
	if s.namedContexts == nil {
		return contextscope.NamedContext{}, contextscope.ErrNamedContextNotFound
	}
	record, err := s.validNamedContextRecord(ctx, input)
	if err != nil {
		return contextscope.NamedContext{}, err
	}
	record, err = s.namedContexts.UpdateNamedContextRecord(ctx, strings.TrimSpace(id), record)
	if err != nil {
		return contextscope.NamedContext{}, err
	}
	return s.namedContextView(ctx, record), nil
}

func (s *Service) DeleteNamedContext(ctx context.Context, id string) error {
	if s.namedContexts == nil {
		return contextscope.ErrNamedContextNotFound
	}
	return s.namedContexts.DeleteNamedContextRecord(ctx, strings.TrimSpace(id))
}

func (s *Service) validNamedContextRecord(
	ctx context.Context,
	input contextscope.NamedContextInput,
) (contextscope.NamedContextRecord, error) {
	input.Title = strings.TrimSpace(input.Title)
	input.Description = strings.TrimSpace(input.Description)
	input.Category = strings.ToLower(strings.TrimSpace(input.Category))
	input.Visibility = strings.ToLower(strings.TrimSpace(input.Visibility))
	input.DefaultScope = strings.ToLower(strings.TrimSpace(input.DefaultScope))
	if input.Visibility == "" {
		input.Visibility = contextscope.VisibilityPersonal
	}
	if input.DefaultScope == "" {
		input.DefaultScope = contextscope.DefaultNone
	}
	if input.Title == "" || len([]rune(input.Title)) > 120 {
		return contextscope.NamedContextRecord{}, errors.New("named context title must contain 1 to 120 characters")
	}
	if len([]rune(input.Description)) > 500 {
		return contextscope.NamedContextRecord{}, errors.New("named context description cannot exceed 500 characters")
	}
	switch input.Category {
	case contextscope.CategoryTeam, contextscope.CategoryProduct,
		contextscope.CategoryServiceFleet, contextscope.CategoryRelease,
		contextscope.CategoryPersonalTask:
	default:
		return contextscope.NamedContextRecord{}, errors.New(
			"named context category must be team, product, service_fleet, release, or personal_task",
		)
	}
	switch input.Visibility {
	case contextscope.VisibilityPersonal, contextscope.VisibilityShared:
	default:
		return contextscope.NamedContextRecord{}, errors.New("named context visibility must be personal or shared")
	}
	switch input.DefaultScope {
	case contextscope.DefaultNone, contextscope.DefaultPersonal, contextscope.DefaultAdministrator:
	default:
		return contextscope.NamedContextRecord{}, errors.New(
			"named context default_scope must be none, personal, or administrator",
		)
	}
	if input.DefaultScope == contextscope.DefaultPersonal &&
		input.Visibility != contextscope.VisibilityPersonal {
		return contextscope.NamedContextRecord{}, errors.New("a personal default must have personal visibility")
	}
	if input.DefaultScope == contextscope.DefaultAdministrator &&
		input.Visibility != contextscope.VisibilityShared {
		return contextscope.NamedContextRecord{}, errors.New("an administrator default must have shared visibility")
	}
	if len(input.Selectors) == 0 {
		return contextscope.NamedContextRecord{}, errors.New("named context must include at least one repository")
	}
	if len(input.Selectors) > contextscope.MaximumContexts {
		return contextscope.NamedContextRecord{}, fmt.Errorf(
			"named context cannot include more than %d repositories",
			contextscope.MaximumContexts,
		)
	}
	for _, selector := range input.Selectors {
		if strings.ToLower(strings.TrimSpace(selector.Kind)) != contextscope.KindRepository ||
			strings.TrimSpace(selector.Path) != "" ||
			strings.TrimSpace(selector.Symbol) != "" ||
			strings.TrimSpace(selector.SymbolKind) != "" ||
			selector.Line != 0 {
			return contextscope.NamedContextRecord{}, errors.New(
				"named contexts represent repository revisions; every selector must be a repository context",
			)
		}
	}
	resolved, err := s.ResolveContexts(ctx, input.Selectors)
	if err != nil {
		return contextscope.NamedContextRecord{}, err
	}
	selectors := make([]contextscope.Selector, 0, len(resolved))
	for _, context := range resolved {
		selectors = append(selectors, selectorFromContext(context))
	}
	return contextscope.NamedContextRecord{
		Title:        input.Title,
		Description:  input.Description,
		Category:     input.Category,
		Visibility:   input.Visibility,
		DefaultScope: input.DefaultScope,
		Selectors:    selectors,
	}, nil
}

func (s *Service) namedContextView(
	ctx context.Context,
	record contextscope.NamedContextRecord,
) contextscope.NamedContext {
	view := contextscope.NamedContext{
		ID:           record.ID,
		Title:        record.Title,
		Description:  record.Description,
		Category:     record.Category,
		Visibility:   record.Visibility,
		DefaultScope: record.DefaultScope,
		OwnerID:      record.OwnerID,
		Managed:      record.Managed,
		Editable:     namedContextEditable(ctx, record),
		State:        "ready",
		URL:          s.namedContextURL(record.ID),
		Contexts:     []contextscope.Context{},
		CreatedAt:    record.CreatedAt,
		UpdatedAt:    record.UpdatedAt,
	}
	contexts, err := s.ResolveContexts(ctx, record.Selectors)
	if err != nil {
		view.State = "invalid"
		var resolution *contextscope.ResolutionError
		if errors.As(err, &resolution) {
			view.Issues = namedContextIssues(record.Title, resolution.Issues)
		} else {
			view.Issues = []contextscope.Issue{{
				Code:    "unavailable",
				Message: "named context could not be resolved",
			}}
		}
		return view
	}
	source := contextscope.Source{
		Kind:  contextscope.SourceNamed,
		ID:    record.ID,
		Title: record.Title,
	}
	for index := range contexts {
		contexts[index].Sources = []contextscope.Source{source}
	}
	view.Contexts = contexts
	return view
}

func namedContextEditable(ctx context.Context, record contextscope.NamedContextRecord) bool {
	viewer, restricted := access.ViewerFromContext(ctx)
	if !restricted {
		return true
	}
	if record.Managed {
		return viewer.Admin
	}
	return record.OwnerID == viewer.ID
}

// ResolveEffectiveContexts returns every effective context and its provenance.
// A stale, missing, unauthorized, or unknown named context fails the request;
// callers never silently widen back to an unscoped search.
func (s *Service) ResolveEffectiveContexts(
	ctx context.Context,
	request contextscope.EffectiveRequest,
) (contextscope.EffectiveResponse, error) {
	output := contextscope.EffectiveResponse{
		Contexts:      []contextscope.Context{},
		NamedContexts: []contextscope.NamedContext{},
	}
	useDefaults := request.UseDefaults == nil || *request.UseDefaults
	requestedIDs := make(map[string]struct{}, len(request.NamedContextIDs))
	for _, id := range request.NamedContextIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return output, errors.New("named_context_ids cannot contain an empty ID")
		}
		requestedIDs[id] = struct{}{}
	}
	var records []contextscope.NamedContextRecord
	if s.namedContexts != nil && (useDefaults || len(requestedIDs) > 0) {
		var err error
		records, err = s.namedContexts.ListNamedContextRecords(ctx)
		if err != nil {
			return output, err
		}
	}
	foundIDs := make(map[string]struct{}, len(requestedIDs))
	type expansion struct {
		record  contextscope.NamedContextRecord
		sources []contextscope.Source
	}
	expansions := make([]expansion, 0)
	for _, record := range records {
		_, selected := requestedIDs[record.ID]
		defaultSource := ""
		if useDefaults {
			switch record.DefaultScope {
			case contextscope.DefaultPersonal:
				defaultSource = contextscope.SourcePersonalDefault
			case contextscope.DefaultAdministrator:
				defaultSource = contextscope.SourceAdministratorDefault
			}
		}
		if !selected && defaultSource == "" {
			continue
		}
		if selected {
			foundIDs[record.ID] = struct{}{}
		}
		sources := make([]contextscope.Source, 0, 2)
		if defaultSource != "" {
			sources = append(sources, contextscope.Source{
				Kind:  defaultSource,
				ID:    record.ID,
				Title: record.Title,
			})
		}
		if selected {
			sources = append(sources, contextscope.Source{
				Kind:  contextscope.SourceNamed,
				ID:    record.ID,
				Title: record.Title,
			})
		}
		expansions = append(expansions, expansion{
			record:  record,
			sources: sources,
		})
	}
	for id := range requestedIDs {
		if _, ok := foundIDs[id]; !ok {
			return output, fmt.Errorf("%w: %s", contextscope.ErrNamedContextNotFound, id)
		}
	}
	expandedCount := len(request.Contexts)
	for _, item := range expansions {
		expandedCount += len(item.record.Selectors)
	}
	if expandedCount > contextscope.MaximumContexts {
		return output, &contextscope.ResolutionError{Issues: []contextscope.Issue{{
			Index:   contextscope.MaximumContexts,
			Code:    "too_many",
			Message: fmt.Sprintf("effective context exceeds the %d-context limit", contextscope.MaximumContexts),
		}}}
	}
	for _, item := range expansions {
		contexts, err := s.ResolveContexts(ctx, item.record.Selectors)
		if err != nil {
			var resolution *contextscope.ResolutionError
			if errors.As(err, &resolution) {
				return output, &contextscope.ResolutionError{
					Issues: namedContextIssues(item.record.Title, resolution.Issues),
				}
			}
			return output, fmt.Errorf("resolve named context %q: %w", item.record.Title, err)
		}
		for index := range contexts {
			contexts[index].Sources = append([]contextscope.Source(nil), item.sources...)
			mergeEffectiveContext(&output.Contexts, contexts[index])
		}
		view := s.namedContextView(ctx, item.record)
		if view.State != "ready" {
			return output, fmt.Errorf("named context %q is invalid", item.record.Title)
		}
		output.NamedContexts = append(output.NamedContexts, view)
	}
	explicit, err := s.ResolveContexts(ctx, request.Contexts)
	if err != nil {
		return output, err
	}
	for index := range explicit {
		explicit[index].Sources = []contextscope.Source{{Kind: contextscope.SourceExplicit}}
		mergeEffectiveContext(&output.Contexts, explicit[index])
	}
	return output, nil
}

func namedContextIssues(title string, issues []contextscope.Issue) []contextscope.Issue {
	output := make([]contextscope.Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.Code == "unavailable" {
			issue.Selector = contextscope.Selector{}
			issue.Message = fmt.Sprintf(
				"named context %q contains a repository that is missing or unavailable to the current viewer",
				title,
			)
		} else {
			issue.Message = fmt.Sprintf("named context %q: %s", title, issue.Message)
		}
		output = append(output, issue)
	}
	return output
}

func mergeEffectiveContext(contexts *[]contextscope.Context, candidate contextscope.Context) {
	key := resolvedContextKey(candidate)
	for index := range *contexts {
		if resolvedContextKey((*contexts)[index]) != key {
			continue
		}
		for _, source := range candidate.Sources {
			duplicate := false
			for _, existing := range (*contexts)[index].Sources {
				if existing.Kind == source.Kind && existing.ID == source.ID {
					duplicate = true
					break
				}
			}
			if !duplicate {
				(*contexts)[index].Sources = append((*contexts)[index].Sources, source)
			}
		}
		return
	}
	*contexts = append(*contexts, candidate)
}

func resolvedContextKey(context contextscope.Context) string {
	return fmt.Sprintf(
		"%s\x00%d\x00%s\x00%s\x00%s\x00%s\x00%d",
		context.Kind,
		context.RepositoryID,
		context.Revision,
		context.Path,
		context.Symbol,
		context.SymbolKind,
		context.Line,
	)
}

func selectorFromContext(context contextscope.Context) contextscope.Selector {
	return contextscope.Selector{
		Kind:         context.Kind,
		RepositoryID: context.RepositoryID,
		Revision:     context.Revision,
		Path:         context.Path,
		Symbol:       context.Symbol,
		SymbolKind:   context.SymbolKind,
		Line:         context.Line,
	}
}

func (s *Service) namedContextURL(id string) string {
	s.mu.RLock()
	baseURL := s.baseURL
	s.mu.RUnlock()
	return baseURL + "/contexts/" + url.PathEscape(id)
}

// ContextURL returns a canonical, copyable URL that round-trips the complete
// structured identity through the browser composer and JSON resolver.
func (s *Service) ContextURL(context contextscope.Context) string {
	values := url.Values{
		"kind":       []string{context.Kind},
		"repository": []string{strconv.FormatInt(context.RepositoryID, 10)},
		"revision":   []string{context.Revision},
	}
	if context.Path != "" {
		values.Set("path", context.Path)
	}
	if context.Symbol != "" {
		values.Set("symbol", context.Symbol)
		values.Set("symbol_kind", context.SymbolKind)
		values.Set("line", strconv.Itoa(context.Line))
	}
	s.mu.RLock()
	baseURL := s.baseURL
	s.mu.RUnlock()
	return baseURL + "/contexts?" + values.Encode()
}
