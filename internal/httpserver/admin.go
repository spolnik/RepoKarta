package httpserver

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/spolnik/RepoKarta/internal/agent"
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
		data := s.adminData(request.Context(), csrf)
		data.Error = err.Error()
		response.WriteHeader(http.StatusBadRequest)
		s.renderAdmin(response, data)
		return
	}
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
		s.renderAdminError(response, "The administrator credentials were not accepted")
		return
	}
	if _, err := s.security.CreateAdminSession(response); err != nil {
		http.Error(response, "Could not create administrator session", http.StatusInternalServerError)
		return
	}
	http.Redirect(response, request, "/admin", http.StatusSeeOther)
}

func (s *Server) adminPage(response http.ResponseWriter, request *http.Request) {
	csrf, ok := s.security.AdminSession(request)
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
		data.Error = err.Error()
		response.WriteHeader(http.StatusBadRequest)
		s.renderAdmin(response, data)
		return
	}
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
		data.Error = err.Error()
		response.WriteHeader(http.StatusConflict)
		s.renderAdmin(response, data)
		return
	}
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
		http.Error(response, "Could not create diagnostics export", http.StatusInternalServerError)
		return
	}
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
	http.Redirect(response, request, "/admin/login", http.StatusSeeOther)
}

func (s *Server) authLogout(response http.ResponseWriter, request *http.Request) {
	s.security.Logout(response, request)
}

func (s *Server) adminData(ctx context.Context, csrf string) adminPageData {
	settings := s.security.Settings()
	data := adminPageData{
		Version:       s.config.Version,
		Authenticated: true,
		CSRFToken:     csrf,
		ProviderError: s.security.ProviderError(),
		AllowOpen:     s.security.AllowOpen(),
		AdminEnabled:  s.security.AdminEnabled(),
		Mode:          string(settings.Mode),
		PublicURL:     settings.PublicURL,
		TeamDomain:    settings.CloudflareTeamDomain,
		Audience:      settings.CloudflareAudience,
		MetadataURL:   settings.SAMLMetadataURL,
		EntityID:      settings.SAMLEntityID,
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
	if !data.AdminEnabled && data.Error == "" {
		data.Error = security.ErrAdminUnavailable.Error()
	}
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(response, "admin", data); err != nil {
		slog.Error("render administrator template", "error", err)
	}
}
