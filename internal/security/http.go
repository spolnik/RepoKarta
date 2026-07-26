package security

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/crewjam/saml/samlsp"
	"github.com/spolnik/RepoKarta/internal/access"
	"github.com/spolnik/RepoKarta/internal/audit"
	"github.com/spolnik/RepoKarta/internal/identity"
)

// Middleware validates Host and Origin, then applies the selected authentication mode.
func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		settings := m.Settings()
		if err := m.validateRequestBoundary(settings, request); err != nil {
			m.recordAuthentication(request, Principal{Provider: string(settings.Mode)}, "failure", err.Error())
			http.Error(response, err.Error(), http.StatusForbidden)
			return
		}
		if isPublicSecurityPath(request.URL.Path) {
			next.ServeHTTP(response, request)
			return
		}
		switch settings.Mode {
		case ModeLocal:
			m.servePrincipal(next, response, request, Principal{
				ID:       "admin",
				Name:     "Local administrator",
				Provider: string(ModeLocal),
				Role:     identity.RoleAdmin,
				Admin:    true,
			})
		case ModeOpen:
			m.servePrincipal(next, response, request, Principal{
				ID:       "anonymous",
				Name:     "Anonymous",
				Provider: string(ModeOpen),
				Role:     identity.RoleReader,
			})
		case ModeCloudflareAccess:
			m.mu.RLock()
			validator := m.cloudflare
			providerError := m.providerError
			m.mu.RUnlock()
			if validator == nil {
				m.recordAuthentication(request, Principal{Provider: string(settings.Mode)}, "failure", "provider unavailable")
				writeProviderUnavailable(response, providerError)
				return
			}
			token := strings.TrimSpace(request.Header.Get("Cf-Access-Jwt-Assertion"))
			if token == "" {
				m.recordAuthentication(request, Principal{Provider: string(settings.Mode)}, "failure", "assertion missing")
				http.Error(response, "Cloudflare Access authentication is required", http.StatusUnauthorized)
				return
			}
			principal, err := validator.Validate(request.Context(), token)
			if err != nil {
				m.recordAuthentication(request, Principal{Provider: string(settings.Mode)}, "failure", "assertion rejected")
				http.Error(response, "Cloudflare Access authentication failed", http.StatusUnauthorized)
				return
			}
			m.servePrincipal(next, response, request, principal)
		case ModeSAML:
			m.mu.RLock()
			middleware := m.samlMiddleware
			providerError := m.providerError
			m.mu.RUnlock()
			if middleware == nil {
				m.recordAuthentication(request, Principal{Provider: string(settings.Mode)}, "failure", "provider unavailable")
				writeProviderUnavailable(response, providerError)
				return
			}
			middleware.RequireAccount(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				principal, ok := principalFromSAML(samlsp.SessionFromContext(request.Context()))
				if !ok {
					m.recordAuthentication(request, Principal{Provider: string(settings.Mode)}, "failure", "stable subject missing")
					http.Error(response, "SAML identity did not include a stable user identifier", http.StatusUnauthorized)
					return
				}
				m.servePrincipal(next, response, request, principal)
			})).ServeHTTP(response, request)
		default:
			m.recordAuthentication(request, Principal{Provider: string(settings.Mode)}, "failure", "mode unavailable")
			http.Error(response, "Authentication mode is not configured", http.StatusServiceUnavailable)
		}
	})
}

func (m *Manager) servePrincipal(next http.Handler, response http.ResponseWriter, request *http.Request, principal Principal) {
	if m.identities != nil && principal.Provider != string(ModeLocal) && principal.Provider != string(ModeOpen) {
		resolution, err := m.identities.ResolveIdentity(request.Context(), identity.Claims{
			Provider: principal.Provider,
			Subject:  principal.ID,
			Email:    principal.Email,
			Name:     principal.Name,
			Groups:   principal.Groups,
		})
		if err != nil {
			outcome := "failure"
			status := http.StatusInternalServerError
			message := "Identity authorization is unavailable"
			if errors.Is(err, identity.ErrDeprovisioned) {
				outcome = "denied"
				status = http.StatusForbidden
				message = "Identity is suspended or deprovisioned"
			}
			m.recordAuthentication(request, principal, outcome, message)
			http.Error(response, message, status)
			return
		}
		principal.Role = resolution.Role
		principal.Admin = resolution.Role == identity.RoleAdmin
		if resolution.User.DisplayName != "" {
			principal.Name = resolution.User.DisplayName
		}
		if resolution.User.Email != "" {
			principal.Email = resolution.User.Email
		}
	}
	if principal.Role == "" {
		principal.Role = identity.RoleReader
	}
	m.recordAuthentication(request, principal, "success", "")
	ctx := withPrincipal(request.Context(), principal)
	ctx = access.WithViewer(ctx, access.Viewer{
		ID:     access.IdentityID(principal.Provider, principal.ID),
		Groups: principal.Groups,
		Admin:  principal.Admin,
	})
	next.ServeHTTP(response, request.WithContext(ctx))
}

func (m *Manager) recordAuthentication(request *http.Request, principal Principal, outcome, reason string) {
	if m.audit == nil {
		return
	}
	actorID := access.IdentityID(principal.Provider, principal.ID)
	if principal.ID == "" {
		actorID = "unknown"
	}
	metadata := map[string]string{"method": request.Method}
	if reason != "" {
		metadata["reason"] = reason
	}
	if err := m.audit.AppendAuditEvent(request.Context(), audit.Event{
		ActorID:       actorID,
		ActorName:     principal.Name,
		Action:        "authentication.validate",
		TargetType:    "request",
		TargetID:      request.URL.Path,
		Outcome:       outcome,
		Provider:      principal.Provider,
		CorrelationID: audit.CorrelationID(request.Context()),
		Metadata:      metadata,
	}); err != nil {
		slog.Error("append authentication audit event", "outcome", outcome, "error", err)
	}
}

func isPublicSecurityPath(path string) bool {
	return path == "/healthz" ||
		path == "/admin" ||
		path == "/admin/login" ||
		strings.HasPrefix(path, "/admin/") ||
		strings.HasPrefix(path, "/assets/") ||
		strings.HasPrefix(path, "/saml/") ||
		strings.HasPrefix(path, "/scim/") ||
		path == "/mcp"
}

func writeProviderUnavailable(response http.ResponseWriter, _ string) {
	http.Error(response, "Authentication provider is unavailable; contact the RepoKarta administrator", http.StatusServiceUnavailable)
}

func (m *Manager) validateRequestBoundary(settings Settings, request *http.Request) error {
	if settings.Mode == ModeLocal {
		_, configuredPort, _ := net.SplitHostPort(m.address)
		host, port, err := net.SplitHostPort(request.Host)
		if err != nil {
			host = request.Host
			port = ""
		}
		if !isLoopbackHost(host) || (configuredPort != "" && port != "" && port != configuredPort) {
			return errors.New("Invalid Host")
		}
		if origin := request.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || !isLoopbackHost(parsed.Hostname()) ||
				(configuredPort != "" && parsed.Port() != "" && parsed.Port() != configuredPort) {
				return errors.New("Invalid Origin")
			}
		}
		return nil
	}
	publicURL, err := url.Parse(settings.PublicURL)
	if err != nil || publicURL.Host == "" {
		return errors.New("Invalid public URL configuration")
	}
	if !strings.EqualFold(request.Host, publicURL.Host) {
		return errors.New("Invalid Host")
	}
	if origin := request.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(parsed.Scheme, publicURL.Scheme) ||
			!strings.EqualFold(parsed.Host, publicURL.Host) {
			return errors.New("Invalid Origin")
		}
	}
	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SAMLHandler serves the service-provider metadata and assertion endpoints.
func (m *Manager) SAMLHandler() http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		m.mu.RLock()
		middleware := m.samlMiddleware
		providerError := m.providerError
		mode := m.settings.Mode
		m.mu.RUnlock()
		if mode != ModeSAML || middleware == nil {
			writeProviderUnavailable(response, providerError)
			return
		}
		middleware.ServeHTTP(response, request)
	})
}

// Logout clears the native SAML session or redirects through Cloudflare Access logout.
func (m *Manager) Logout(response http.ResponseWriter, request *http.Request) {
	settings := m.Settings()
	if settings.Mode == ModeSAML {
		m.mu.RLock()
		middleware := m.samlMiddleware
		m.mu.RUnlock()
		if middleware != nil {
			_ = middleware.Session.DeleteSession(response, request)
		}
	}
	target := "/"
	if settings.Mode == ModeCloudflareAccess && settings.PublicURL != "" {
		if parsed, err := url.Parse(settings.PublicURL); err == nil {
			parsed.Path = "/cdn-cgi/access/logout"
			parsed.RawQuery = ""
			parsed.Fragment = ""
			target = parsed.String()
		}
	}
	http.Redirect(response, request, target, http.StatusSeeOther)
}
