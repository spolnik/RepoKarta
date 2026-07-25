package httpserver

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/spolnik/RepoKarta/internal/security"
)

type adminPageData struct {
	Version       string
	Authenticated bool
	CSRFToken     string
	Error         string
	Notice        string
	ProviderError string
	AllowOpen     bool
	AdminEnabled  bool
	Mode          string
	PublicURL     string
	TeamDomain    string
	Audience      string
	MetadataURL   string
	EntityID      string
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
	s.renderAdmin(response, s.adminData(csrf))
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
		data := s.adminData(csrf)
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
	data := s.adminData(csrf)
	data.Notice = "Authentication settings saved and activated."
	s.renderAdmin(response, data)
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

func (s *Server) adminData(csrf string) adminPageData {
	settings := s.security.Settings()
	return adminPageData{
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
}

func (s *Server) renderAdminError(response http.ResponseWriter, message string) {
	response.WriteHeader(http.StatusUnauthorized)
	s.renderAdmin(response, adminPageData{
		Version:      s.config.Version,
		AdminEnabled: s.security.AdminEnabled(),
		Error:        message,
	})
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
