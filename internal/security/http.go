package security

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/crewjam/saml/samlsp"
)

// Middleware validates Host and Origin, then applies the selected authentication mode.
func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		settings := m.Settings()
		if err := m.validateRequestBoundary(settings, request); err != nil {
			http.Error(response, err.Error(), http.StatusForbidden)
			return
		}
		if isPublicSecurityPath(request.URL.Path) {
			next.ServeHTTP(response, request)
			return
		}
		switch settings.Mode {
		case ModeLocal:
			next.ServeHTTP(response, request)
		case ModeOpen:
			next.ServeHTTP(response, request.WithContext(withPrincipal(request.Context(), Principal{
				ID:       "anonymous",
				Name:     "Anonymous",
				Provider: string(ModeOpen),
			})))
		case ModeCloudflareAccess:
			m.mu.RLock()
			validator := m.cloudflare
			providerError := m.providerError
			m.mu.RUnlock()
			if validator == nil {
				writeProviderUnavailable(response, providerError)
				return
			}
			token := strings.TrimSpace(request.Header.Get("Cf-Access-Jwt-Assertion"))
			if token == "" {
				http.Error(response, "Cloudflare Access authentication is required", http.StatusUnauthorized)
				return
			}
			principal, err := validator.Validate(request.Context(), token)
			if err != nil {
				http.Error(response, "Cloudflare Access authentication failed", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(response, request.WithContext(withPrincipal(request.Context(), principal)))
		case ModeSAML:
			m.mu.RLock()
			middleware := m.samlMiddleware
			providerError := m.providerError
			m.mu.RUnlock()
			if middleware == nil {
				writeProviderUnavailable(response, providerError)
				return
			}
			middleware.RequireAccount(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				principal, ok := principalFromSAML(samlsp.SessionFromContext(request.Context()))
				if !ok {
					http.Error(response, "SAML identity did not include a stable user identifier", http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(response, request.WithContext(withPrincipal(request.Context(), principal)))
			})).ServeHTTP(response, request)
		default:
			http.Error(response, "Authentication mode is not configured", http.StatusServiceUnavailable)
		}
	})
}

func isPublicSecurityPath(path string) bool {
	return path == "/healthz" ||
		path == "/admin" ||
		path == "/admin/login" ||
		strings.HasPrefix(path, "/admin/") ||
		strings.HasPrefix(path, "/assets/") ||
		strings.HasPrefix(path, "/saml/") ||
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
