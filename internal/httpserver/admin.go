package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/spolnik/RepoKarta/internal/acquisition"
	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/audit"
	"github.com/spolnik/RepoKarta/internal/identity"
	"github.com/spolnik/RepoKarta/internal/maintenance"
	"github.com/spolnik/RepoKarta/internal/security"
	"github.com/spolnik/RepoKarta/internal/store"
)

type adminPageData struct {
	Version               string
	Authenticated         bool
	CSRFToken             string
	Error                 string
	Notice                string
	ProviderError         string
	AllowOpen             bool
	AdminEnabled          bool
	Mode                  string
	PublicURL             string
	TeamDomain            string
	Audience              string
	MetadataURL           string
	EntityID              string
	MaintenanceAvailable  bool
	Storage               maintenance.Inventory
	StorageError          string
	CleanupPlan           *maintenance.CleanupPlan
	RepositoryAccess      []store.RepositoryAccess
	RepositoryAccessError string
	EnterpriseAvailable   bool
	EnterpriseError       string
	Users                 []identity.User
	Groups                []identity.Group
	RoleMappings          []identity.RoleMapping
	AuditRetention        audit.Retention
	RecentAudit           []audit.Event
	Acquisitions          []acquisition.Repository
	AcquisitionError      string
	DiscoveryCandidates   []acquisition.Candidate
	DiscoverProvider      string
	DiscoverLocation      string
	DiscoverCredentialRef string
	IncludeArchived       bool
	IncludeForks          bool
	IncludePrivate        bool
	DiscoverTeam          string
	DiscoverTopics        string
	DiscoverAllow         string
	DiscoverDeny          string
}

func (s *Server) discoverRepositories(response http.ResponseWriter, request *http.Request) {
	csrf, ok := s.validAdminForm(response, request, 64<<10)
	if !ok {
		return
	}
	discovery := acquisition.DiscoverRequest{
		Provider:        request.FormValue("provider"),
		Location:        request.FormValue("location"),
		CredentialRef:   request.FormValue("credential_ref"),
		IncludeArchived: request.FormValue("include_archived") == "true",
		IncludeForks:    request.FormValue("include_forks") == "true",
		IncludePrivate:  request.FormValue("include_private") == "true",
		Team:            request.FormValue("team"),
		Topics:          splitAccessSubjects(request.FormValue("topics")),
		Allow:           splitAccessSubjects(request.FormValue("allow")),
		Deny:            splitAccessSubjects(request.FormValue("deny")),
	}
	data := s.adminData(request.Context(), csrf)
	data.DiscoverProvider = discovery.Provider
	data.DiscoverLocation = discovery.Location
	data.DiscoverCredentialRef = discovery.CredentialRef
	data.IncludeArchived = discovery.IncludeArchived
	data.IncludeForks = discovery.IncludeForks
	data.IncludePrivate = discovery.IncludePrivate
	data.DiscoverTeam = discovery.Team
	data.DiscoverTopics = strings.Join(discovery.Topics, ", ")
	data.DiscoverAllow = strings.Join(discovery.Allow, ", ")
	data.DiscoverDeny = strings.Join(discovery.Deny, ", ")
	candidates, err := s.repositoryAcquisition.Discover(request.Context(), discovery)
	if err != nil {
		data.Error = err.Error()
		response.WriteHeader(http.StatusBadRequest)
		s.renderAdmin(response, data)
		return
	}
	data.DiscoveryCandidates = candidates
	data.Notice = fmt.Sprintf("Discovery preview contains %d repositories. Review exclusions and approve repositories individually.", len(candidates))
	s.renderAdmin(response, data)
}

func (s *Server) acquireRepository(response http.ResponseWriter, request *http.Request) {
	csrf, ok := s.validAdminForm(response, request, 64<<10)
	if !ok {
		return
	}
	candidate := acquisition.Candidate{
		Provider:             request.FormValue("provider"),
		ProviderRepositoryID: request.FormValue("provider_repository_id"),
		CanonicalID:          request.FormValue("canonical_id"),
		Name:                 request.FormValue("name"),
		Namespace:            request.FormValue("namespace"),
		RemoteURL:            request.FormValue("remote_url"),
		WebURL:               request.FormValue("web_url"),
		LocalPath:            request.FormValue("local_path"),
		DefaultBranch:        request.FormValue("default_branch"),
		Visibility:           request.FormValue("visibility"),
		Archived:             request.FormValue("archived") == "true",
		Forked:               request.FormValue("forked") == "true",
		InclusionPolicy:      request.FormValue("inclusion_policy"),
	}
	repository, err := s.repositoryAcquisition.Acquire(
		request.Context(),
		candidate,
		request.FormValue("credential_ref"),
	)
	data := s.adminData(request.Context(), csrf)
	if err != nil {
		data.Error = err.Error()
		response.WriteHeader(http.StatusBadRequest)
		s.renderAdmin(response, data)
		return
	}
	data.Notice = repository.CanonicalID + " was acquired and queued for commit-pinned indexing."
	s.renderAdmin(response, data)
}

func (s *Server) syncAcquiredRepository(response http.ResponseWriter, request *http.Request) {
	csrf, ok := s.validAdminForm(response, request, 32<<10)
	if !ok {
		return
	}
	id, err := parseAcquisitionID(request.FormValue("repository_id"))
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	repository, err := s.repositoryAcquisition.Sync(request.Context(), id)
	data := s.adminData(request.Context(), csrf)
	if err != nil {
		data.Error = err.Error()
		response.WriteHeader(http.StatusConflict)
		s.renderAdmin(response, data)
		return
	}
	data.Notice = repository.CanonicalID + " synchronized at " + shortCommit(repository.HeadCommit) + "."
	s.renderAdmin(response, data)
}

func (s *Server) removeAcquiredRepository(response http.ResponseWriter, request *http.Request) {
	csrf, ok := s.validAdminForm(response, request, 32<<10)
	if !ok {
		return
	}
	if request.FormValue("confirm") != "remove" {
		http.Error(response, "Confirm repository removal", http.StatusBadRequest)
		return
	}
	id, err := parseAcquisitionID(request.FormValue("repository_id"))
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	removedPath, err := s.repositoryAcquisition.Remove(request.Context(), id)
	data := s.adminData(request.Context(), csrf)
	if err != nil {
		data.Error = err.Error()
		response.WriteHeader(http.StatusConflict)
		s.renderAdmin(response, data)
		return
	}
	if removedPath == "" {
		data.Notice = "Local repository registration removed. No user-owned source files were changed."
	} else {
		data.Notice = "RepoKarta-owned checkout moved to recoverable trash: " + removedPath
	}
	s.renderAdmin(response, data)
}

func (s *Server) validAdminForm(response http.ResponseWriter, request *http.Request, maximumBytes int64) (string, bool) {
	request.Body = http.MaxBytesReader(response, request.Body, maximumBytes)
	if err := request.ParseForm(); err != nil {
		http.Error(response, "Invalid administrator request", http.StatusBadRequest)
		return "", false
	}
	csrf, ok := s.security.AdminSession(request)
	if !ok {
		http.Redirect(response, request, "/admin/login", http.StatusSeeOther)
		return "", false
	}
	if !s.security.ValidAdminCSRF(request, request.FormValue("csrf")) {
		http.Error(response, "Invalid administrator CSRF token", http.StatusForbidden)
		return "", false
	}
	return csrf, true
}

func parseAcquisitionID(value string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid repository acquisition")
	}
	return id, nil
}

func (s *Server) updateRepositoryAccess(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	if err := request.ParseForm(); err != nil {
		http.Error(response, "Invalid repository access request", http.StatusBadRequest)
		return
	}
	csrf, ok := s.security.AdminSession(request)
	if !ok {
		http.Redirect(response, request, "/admin/login", http.StatusSeeOther)
		return
	}
	if !s.security.ValidAdminCSRF(request, request.FormValue("csrf")) {
		http.Error(response, "Invalid administrator CSRF token", http.StatusForbidden)
		return
	}
	repositoryID, err := strconv.ParseInt(request.FormValue("repository_id"), 10, 64)
	if err != nil || repositoryID <= 0 {
		http.Error(response, "Invalid repository", http.StatusBadRequest)
		return
	}
	policy := store.RepositoryAccess{
		RepositoryID: repositoryID,
		OwnerID:      request.FormValue("owner_id"),
		Visibility:   request.FormValue("visibility"),
		Users:        splitAccessSubjects(request.FormValue("users")),
		Groups:       splitAccessSubjects(request.FormValue("groups")),
	}
	if err := s.repositoryAccess.SetRepositoryAccess(request.Context(), policy); err != nil {
		s.recordAdminEvent(request, "repository.access.update", "repository", strconv.FormatInt(repositoryID, 10), "failure", nil)
		data := s.adminData(request.Context(), csrf)
		data.Error = err.Error()
		response.WriteHeader(http.StatusBadRequest)
		s.renderAdmin(response, data)
		return
	}
	s.recordAdminEvent(request, "repository.access.update", "repository", strconv.FormatInt(repositoryID, 10), "success", map[string]string{
		"owner": policy.OwnerID, "visibility": policy.Visibility,
	})
	data := s.adminData(request.Context(), csrf)
	data.Notice = "Repository access saved. Source and every derived artifact now use this policy."
	s.renderAdmin(response, data)
}

func splitAccessSubjects(value string) []string {
	return strings.FieldsFunc(value, func(character rune) bool {
		return character == ',' || character == '\r' || character == '\n'
	})
}

func (s *Server) adminLoginPage(response http.ResponseWriter, request *http.Request) {
	if _, ok := s.security.AdminSession(request); ok {
		http.Redirect(response, request, "/admin", http.StatusSeeOther)
		return
	}
	s.renderAdmin(response, adminPageData{
		Version:      s.config.Version,
		AdminEnabled: s.security.AdminEnabled(),
	})
}

func (s *Server) adminLogin(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if err := request.ParseForm(); err != nil {
		s.renderAdminError(response, "Invalid sign-in request")
		return
	}
	if !s.security.AuthenticateAdmin(request.FormValue("username"), request.FormValue("password")) {
		s.recordAdminEvent(request, "authentication.bootstrap", "administrator-session", "login", "failure", nil)
		s.renderAdminError(response, "The administrator credentials were not accepted")
		return
	}
	if _, err := s.security.CreateAdminSession(response); err != nil {
		s.recordAdminEvent(request, "authentication.bootstrap", "administrator-session", "login", "failure", nil)
		http.Error(response, "Could not create administrator session", http.StatusInternalServerError)
		return
	}
	s.recordAdminEvent(request, "authentication.bootstrap", "administrator-session", "login", "success", nil)
	http.Redirect(response, request, "/admin", http.StatusSeeOther)
}

func (s *Server) adminPage(response http.ResponseWriter, request *http.Request) {
	csrf, ok := s.security.AdminSession(request)
	if !ok && s.security.Mode() == security.ModeLocal {
		var err error
		csrf, err = s.security.CreateAdminSession(response)
		ok = err == nil
		if err != nil {
			slog.Error("create local administrator session", "error", err)
			http.Error(response, "Could not create local administrator session", http.StatusInternalServerError)
			return
		}
	}
	if !ok {
		http.Redirect(response, request, "/admin/login", http.StatusSeeOther)
		return
	}
	s.renderAdmin(response, s.adminData(request.Context(), csrf))
}

func (s *Server) updateSecurity(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 32<<10)
	if err := request.ParseForm(); err != nil {
		http.Error(response, "Invalid security configuration", http.StatusBadRequest)
		return
	}
	csrf, ok := s.security.AdminSession(request)
	if !ok {
		http.Redirect(response, request, "/admin/login", http.StatusSeeOther)
		return
	}
	if !s.security.ValidAdminCSRF(request, request.FormValue("csrf")) {
		http.Error(response, "Invalid administrator CSRF token", http.StatusForbidden)
		return
	}
	settings := security.Settings{
		Mode:                 security.Mode(request.FormValue("mode")),
		PublicURL:            request.FormValue("public_url"),
		CloudflareTeamDomain: request.FormValue("cloudflare_team_domain"),
		CloudflareAudience:   request.FormValue("cloudflare_audience"),
		SAMLMetadataURL:      request.FormValue("saml_metadata_url"),
		SAMLEntityID:         request.FormValue("saml_entity_id"),
	}
	if err := s.security.UpdateSettings(request.Context(), settings); err != nil {
		s.recordAdminEvent(request, "security.settings.update", "security-configuration", string(settings.Mode), "failure", nil)
		data := s.adminData(request.Context(), csrf)
		data.Error = err.Error()
		data.Mode = string(settings.Mode)
		data.PublicURL = strings.TrimSpace(settings.PublicURL)
		data.TeamDomain = strings.TrimSpace(settings.CloudflareTeamDomain)
		data.Audience = strings.TrimSpace(settings.CloudflareAudience)
		data.MetadataURL = strings.TrimSpace(settings.SAMLMetadataURL)
		data.EntityID = strings.TrimSpace(settings.SAMLEntityID)
		response.WriteHeader(http.StatusBadRequest)
		s.renderAdmin(response, data)
		return
	}
	s.recordAdminEvent(request, "security.settings.update", "security-configuration", string(settings.Mode), "success", map[string]string{
		"mode": string(settings.Mode), "public_url": settings.PublicURL,
	})
	data := s.adminData(request.Context(), csrf)
	data.Notice = "Authentication settings saved and activated."
	s.renderAdmin(response, data)
}

func (s *Server) previewCleanup(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 32<<10)
	if err := request.ParseForm(); err != nil {
		http.Error(response, "Invalid cleanup request", http.StatusBadRequest)
		return
	}
	csrf, ok := s.security.AdminSession(request)
	if !ok {
		http.Redirect(response, request, "/admin/login", http.StatusSeeOther)
		return
	}
	if !s.security.ValidAdminCSRF(request, request.FormValue("csrf")) {
		http.Error(response, "Invalid administrator CSRF token", http.StatusForbidden)
		return
	}
	data := s.adminData(request.Context(), csrf)
	plan, err := s.maintenance.Plan(request.Context(), request.Form["target"])
	if err != nil {
		s.recordAdminEvent(request, "owned-data.cleanup.preview", "storage", "cleanup-plan", "failure", nil)
		data.Error = err.Error()
		response.WriteHeader(http.StatusBadRequest)
		s.renderAdmin(response, data)
		return
	}
	s.recordAdminEvent(request, "owned-data.cleanup.preview", "storage", "cleanup-plan", "success", map[string]string{
		"planned_items": strconv.Itoa(len(plan.Items)),
		"planned_bytes": strconv.FormatInt(plan.TotalBytes, 10),
	})
	data.CleanupPlan = &plan
	data.Notice = "Cleanup preview is ready. Review every exact target before confirming."
	s.renderAdmin(response, data)
}

func (s *Server) executeCleanup(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 32<<10)
	if err := request.ParseForm(); err != nil {
		http.Error(response, "Invalid cleanup request", http.StatusBadRequest)
		return
	}
	csrf, ok := s.security.AdminSession(request)
	if !ok {
		http.Redirect(response, request, "/admin/login", http.StatusSeeOther)
		return
	}
	if !s.security.ValidAdminCSRF(request, request.FormValue("csrf")) {
		http.Error(response, "Invalid administrator CSRF token", http.StatusForbidden)
		return
	}
	data := s.adminData(request.Context(), csrf)
	if request.FormValue("confirm") != "remove" {
		data.Error = "Confirm the reviewed cleanup plan before removing files."
		response.WriteHeader(http.StatusBadRequest)
		s.renderAdmin(response, data)
		return
	}
	result, err := s.maintenance.Execute(
		request.Context(),
		request.Form["target"],
		request.FormValue("plan_token"),
	)
	if err != nil {
		s.recordAdminEvent(request, "owned-data.cleanup", "storage", "cleanup-plan", "failure", nil)
		data.Error = err.Error()
		response.WriteHeader(http.StatusConflict)
		s.renderAdmin(response, data)
		return
	}
	s.recordAdminEvent(request, "owned-data.cleanup", "storage", "cleanup-plan", "success", map[string]string{
		"removed_items": strconv.Itoa(result.RemovedItems),
		"removed_bytes": strconv.FormatInt(result.RemovedBytes, 10),
	})
	data = s.adminData(request.Context(), csrf)
	data.Notice = "Cleanup completed: removed " + formatItemCount(result.RemovedItems) +
		" and reclaimed " + formatBytes(result.RemovedBytes) + "."
	s.renderAdmin(response, data)
}

func (s *Server) exportDiagnostics(response http.ResponseWriter, request *http.Request) {
	if _, ok := s.security.AdminSession(request); !ok {
		http.Redirect(response, request, "/admin/login", http.StatusSeeOther)
		return
	}
	settings := s.security.Settings()
	var providers []agent.Status
	if s.agents != nil {
		providers = s.agents.Statuses(request.Context())
	}
	content, name, err := s.maintenance.Diagnostics(request.Context(), maintenance.DiagnosticContext{
		AuthMode:         string(settings.Mode),
		PublicURL:        settings.PublicURL,
		AllowOpen:        s.security.AllowOpen(),
		ProviderStatuses: providers,
	})
	if err != nil {
		s.recordAdminEvent(request, "administration.diagnostics.export", "diagnostics", "bundle", "failure", nil)
		http.Error(response, "Could not create diagnostics export", http.StatusInternalServerError)
		return
	}
	s.recordAdminEvent(request, "administration.diagnostics.export", "diagnostics", name, "success", nil)
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Content-Type", "application/zip")
	response.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(content)
}

func (s *Server) adminLogout(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if err := request.ParseForm(); err != nil {
		http.Error(response, "Invalid sign-out request", http.StatusBadRequest)
		return
	}
	if !s.security.ValidAdminCSRF(request, request.FormValue("csrf")) {
		http.Error(response, "Invalid administrator CSRF token", http.StatusForbidden)
		return
	}
	s.security.DeleteAdminSession(response, request)
	s.recordAdminEvent(request, "authentication.bootstrap.logout", "administrator-session", "logout", "success", nil)
	http.Redirect(response, request, "/admin/login", http.StatusSeeOther)
}

func (s *Server) authLogout(response http.ResponseWriter, request *http.Request) {
	s.security.Logout(response, request)
}

func (s *Server) adminData(ctx context.Context, csrf string) adminPageData {
	settings := s.security.Settings()
	data := adminPageData{
		Version:          s.config.Version,
		Authenticated:    true,
		CSRFToken:        csrf,
		ProviderError:    s.security.ProviderError(),
		AllowOpen:        s.security.AllowOpen(),
		AdminEnabled:     s.security.AdminEnabled(),
		Mode:             string(settings.Mode),
		PublicURL:        settings.PublicURL,
		TeamDomain:       settings.CloudflareTeamDomain,
		Audience:         settings.CloudflareAudience,
		MetadataURL:      settings.SAMLMetadataURL,
		EntityID:         settings.SAMLEntityID,
		DiscoverProvider: "local",
		IncludePrivate:   true,
	}
	if s.maintenance != nil {
		data.MaintenanceAvailable = true
		inventory, err := s.maintenance.Inventory(ctx)
		if err != nil {
			data.StorageError = err.Error()
		} else {
			data.Storage = inventory
		}
	}
	if s.repositoryAccess != nil {
		policies, err := s.repositoryAccess.ListRepositoryAccess(ctx)
		if err != nil {
			data.RepositoryAccessError = err.Error()
		} else {
			data.RepositoryAccess = policies
		}
	}
	if s.enterprise != nil {
		data.EnterpriseAvailable = true
		users, _, usersErr := s.enterprise.ListUsers(ctx, 0, 500)
		groups, _, groupsErr := s.enterprise.ListGroups(ctx, 0, 500)
		mappings, mappingsErr := s.enterprise.ListRoleMappings(ctx)
		retention, retentionErr := s.enterprise.AuditRetention(ctx)
		recent, auditErr := s.enterprise.AuditEvents(ctx, audit.Filter{Limit: 25})
		for _, err := range []error{usersErr, groupsErr, mappingsErr, retentionErr, auditErr} {
			if err != nil {
				data.EnterpriseError = err.Error()
				break
			}
		}
		if data.EnterpriseError == "" {
			data.Users = users
			data.Groups = groups
			data.RoleMappings = mappings
			data.AuditRetention = retention
			data.RecentAudit = recent.Events
		}
	}
	if s.repositoryAcquisition != nil {
		repositories, err := s.repositoryAcquisition.List(ctx)
		if err != nil {
			data.AcquisitionError = err.Error()
		} else {
			data.Acquisitions = repositories
		}
	}
	return data
}

func (s *Server) renderAdminError(response http.ResponseWriter, message string) {
	response.WriteHeader(http.StatusUnauthorized)
	s.renderAdmin(response, adminPageData{
		Version:      s.config.Version,
		AdminEnabled: s.security.AdminEnabled(),
		Error:        message,
	})
}

func formatBytes(value int64) string {
	const unit = int64(1024)
	if value < unit {
		return strconv.FormatInt(value, 10) + " B"
	}
	divisor := unit
	suffix := "KiB"
	for _, candidate := range []string{"MiB", "GiB", "TiB"} {
		if value < divisor*unit {
			break
		}
		divisor *= unit
		suffix = candidate
	}
	return fmt.Sprintf("%.1f %s", float64(value)/float64(divisor), suffix)
}

func formatItemCount(count int) string {
	if count == 1 {
		return "1 item"
	}
	return strconv.Itoa(count) + " items"
}

func (s *Server) renderAdmin(response http.ResponseWriter, data adminPageData) {
	if s.security == nil {
		http.Error(response, "Administrator interface is unavailable", http.StatusNotFound)
		return
	}
	if !data.AdminEnabled && !data.Authenticated && data.Error == "" {
		data.Error = security.ErrAdminUnavailable.Error()
	}
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(response, "admin", data); err != nil {
		slog.Error("render administrator template", "error", err)
	}
}
