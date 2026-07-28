// Package security owns RepoKarta's deployment authentication boundary.
package security

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/crewjam/saml/samlsp"
	"github.com/spolnik/RepoKarta/internal/audit"
	"github.com/spolnik/RepoKarta/internal/identity"
)

type Mode string

const (
	ModeLocal            Mode = "local"
	ModeCloudflareAccess Mode = "cloudflare-access"
	ModeSAML             Mode = "saml"
	ModeOpen             Mode = "open"

	settingsKey          = "security_configuration_v1"
	adminCookieName      = "repokarta_admin_session"
	adminSessionLifetime = 12 * time.Hour
	minimumAdminPassword = 12
)

var ErrAdminUnavailable = errors.New("bootstrap administrator credentials are not configured")

// Settings contains non-secret, administrator-managed security configuration.
type Settings struct {
	Mode                 Mode   `json:"mode"`
	PublicURL            string `json:"public_url,omitempty"`
	CloudflareTeamDomain string `json:"cloudflare_team_domain,omitempty"`
	CloudflareAudience   string `json:"cloudflare_audience,omitempty"`
	SAMLMetadataURL      string `json:"saml_metadata_url,omitempty"`
	SAMLEntityID         string `json:"saml_entity_id,omitempty"`
}

// Store persists non-secret application settings.
type Store interface {
	AppSetting(context.Context, string) (string, bool, error)
	SetAppSetting(context.Context, string, string) error
}

// Config contains startup-only policy and bootstrap credentials.
type Config struct {
	Address       string
	DataDirectory string
	AllowOpen     bool
	AdminUser     string
	AdminPassword string
	Initial       Settings
	HTTPClient    *http.Client
	Identities    identity.Store
	Audit         audit.Recorder
}

// Principal is the authenticated identity attached to an application request.
type Principal struct {
	ID       string
	Email    string
	Name     string
	Provider string
	Groups   []string
	Role     identity.Role
	Admin    bool
}

type principalContextKey struct{}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}

func withPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

type adminSession struct {
	ExpiresAt time.Time
	CSRFToken string
}

type adminLoginAttempt struct {
	Failures     int
	BlockedUntil time.Time
	LastAttempt  time.Time
}

// Manager applies the active mode and owns ephemeral administrator sessions.
type Manager struct {
	store         Store
	address       string
	dataDirectory string
	allowOpen     bool
	adminUser     string
	adminPassword [32]byte
	adminEnabled  bool
	httpClient    *http.Client
	identities    identity.Store
	audit         audit.Recorder

	mu             sync.RWMutex
	settings       Settings
	providerError  string
	cloudflare     *CloudflareValidator
	samlMiddleware *samlsp.Middleware
	adminSessions  map[[32]byte]adminSession
	adminAttempts  map[string]adminLoginAttempt
	changeHandler  func(Settings)
}

func New(ctx context.Context, store Store, config Config) (*Manager, error) {
	if store == nil {
		return nil, errors.New("security settings store is required")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	manager := &Manager{
		store:         store,
		address:       config.Address,
		dataDirectory: config.DataDirectory,
		allowOpen:     config.AllowOpen,
		adminUser:     strings.TrimSpace(config.AdminUser),
		httpClient:    config.HTTPClient,
		identities:    config.Identities,
		audit:         config.Audit,
		adminSessions: make(map[[32]byte]adminSession),
		adminAttempts: make(map[string]adminLoginAttempt),
	}
	if manager.adminUser != "" && config.AdminPassword != "" {
		if len(config.AdminPassword) < minimumAdminPassword {
			return nil, fmt.Errorf("bootstrap administrator password must contain at least %d characters", minimumAdminPassword)
		}
		manager.adminPassword = sha256.Sum256([]byte(config.AdminPassword))
		manager.adminEnabled = true
	}

	settings := normalizeSettings(config.Initial)
	if raw, ok, err := store.AppSetting(ctx, settingsKey); err != nil {
		return nil, err
	} else if ok {
		if err := json.Unmarshal([]byte(raw), &settings); err != nil {
			return nil, fmt.Errorf("decode security configuration: %w", err)
		}
		settings = normalizeSettings(settings)
	}
	if settings.Mode == "" {
		settings.Mode = ModeLocal
	}
	if settings.Mode == ModeOpen && !config.AllowOpen {
		return nil, errors.New("persisted open mode is blocked; restart with -allow-open=true or choose another mode")
	}
	if settings.Mode != ModeLocal && !manager.adminEnabled {
		return nil, ErrAdminUnavailable
	}
	manager.settings = settings
	manager.configureProvider(ctx, settings)
	return manager, nil
}

func normalizeSettings(settings Settings) Settings {
	settings.Mode = Mode(strings.TrimSpace(string(settings.Mode)))
	settings.PublicURL = strings.TrimRight(strings.TrimSpace(settings.PublicURL), "/")
	settings.CloudflareTeamDomain = strings.TrimRight(strings.TrimSpace(settings.CloudflareTeamDomain), "/")
	settings.CloudflareAudience = strings.TrimSpace(settings.CloudflareAudience)
	settings.SAMLMetadataURL = strings.TrimSpace(settings.SAMLMetadataURL)
	settings.SAMLEntityID = strings.TrimSpace(settings.SAMLEntityID)
	if settings.CloudflareTeamDomain != "" && !strings.Contains(settings.CloudflareTeamDomain, "://") {
		settings.CloudflareTeamDomain = "https://" + settings.CloudflareTeamDomain
	}
	return settings
}

func validateSettings(settings Settings, allowOpen bool) error {
	switch settings.Mode {
	case ModeLocal:
		return nil
	case ModeOpen:
		if !allowOpen {
			return errors.New("open mode is disabled by startup policy")
		}
	case ModeCloudflareAccess:
		if settings.CloudflareTeamDomain == "" || settings.CloudflareAudience == "" {
			return errors.New("Cloudflare Access requires a team domain and application audience")
		}
		teamURL, err := validatedHTTPSURL(settings.CloudflareTeamDomain, "Cloudflare team domain")
		if err != nil {
			return err
		}
		if teamURL.Path != "" && teamURL.Path != "/" {
			return errors.New("Cloudflare team domain must not contain a path")
		}
	case ModeSAML:
		if settings.SAMLMetadataURL == "" {
			return errors.New("SAML mode requires an IdP metadata URL")
		}
		if _, err := validatedHTTPSURL(settings.SAMLMetadataURL, "SAML metadata URL"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown authentication mode %q", settings.Mode)
	}
	if settings.PublicURL == "" {
		return errors.New("shared authentication modes require the public RepoKarta URL")
	}
	publicURL, err := validatedHTTPSURL(settings.PublicURL, "public URL")
	if err != nil {
		return err
	}
	if publicURL.Path != "" && publicURL.Path != "/" {
		return errors.New("public URL must not contain a path")
	}
	return nil
}

func validatedHTTPSURL(value, label string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return nil, fmt.Errorf("%s must be an absolute HTTPS URL", label)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("%s must not contain a query or fragment", label)
	}
	return parsed, nil
}

func (m *Manager) Settings() Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.settings
}

func (m *Manager) Mode() Mode {
	return m.Settings().Mode
}

func (m *Manager) AllowOpen() bool {
	return m.allowOpen
}

func (m *Manager) AdminEnabled() bool {
	return m.adminEnabled
}

func (m *Manager) ProviderError() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.providerError
}

func (m *Manager) UpdateSettings(ctx context.Context, settings Settings) error {
	settings = normalizeSettings(settings)
	if err := validateSettings(settings, m.allowOpen); err != nil {
		return err
	}
	if settings.Mode != ModeLocal && !m.adminEnabled {
		return ErrAdminUnavailable
	}
	cloudflare, samlMiddleware, err := m.buildProvider(ctx, settings)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if err := m.store.SetAppSetting(ctx, settingsKey, string(raw)); err != nil {
		return err
	}
	m.mu.Lock()
	m.settings = settings
	m.cloudflare = cloudflare
	m.samlMiddleware = samlMiddleware
	m.providerError = ""
	changeHandler := m.changeHandler
	m.mu.Unlock()
	if changeHandler != nil {
		changeHandler(settings)
	}
	return nil
}

// SetChangeHandler registers the in-process consumers that depend on the
// effective public URL. The handler is called after a successful persisted
// configuration change.
func (m *Manager) SetChangeHandler(handler func(Settings)) {
	m.mu.Lock()
	m.changeHandler = handler
	m.mu.Unlock()
}

func (m *Manager) configureProvider(ctx context.Context, settings Settings) {
	if err := validateSettings(settings, m.allowOpen); err != nil {
		m.providerError = err.Error()
		return
	}
	cloudflare, samlMiddleware, err := m.buildProvider(ctx, settings)
	if err != nil {
		m.providerError = err.Error()
		return
	}
	m.cloudflare = cloudflare
	m.samlMiddleware = samlMiddleware
	m.providerError = ""
}

func (m *Manager) buildProvider(ctx context.Context, settings Settings) (*CloudflareValidator, *samlsp.Middleware, error) {
	switch settings.Mode {
	case ModeCloudflareAccess:
		return NewCloudflareValidator(settings.CloudflareTeamDomain, settings.CloudflareAudience, m.httpClient), nil, nil
	case ModeSAML:
		middleware, err := buildSAMLMiddleware(ctx, settings, m.dataDirectory, m.httpClient)
		return nil, middleware, err
	default:
		return nil, nil, nil
	}
}

func randomToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (m *Manager) AuthenticateAdmin(username, password string) bool {
	if !m.adminEnabled {
		return false
	}
	usernameOK := subtle.ConstantTimeCompare([]byte(strings.TrimSpace(username)), []byte(m.adminUser))
	passwordHash := sha256.Sum256([]byte(password))
	passwordOK := subtle.ConstantTimeCompare(passwordHash[:], m.adminPassword[:])
	return usernameOK&passwordOK == 1
}

// AdminLoginRetryAfter returns the remaining source-specific bootstrap-login
// backoff without trusting proxy-controlled forwarding headers.
func (m *Manager) AdminLoginRetryAfter(source string) time.Duration {
	source = strings.TrimSpace(source)
	if source == "" {
		source = "unknown"
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneAdminAttempts(now)
	attempt := m.adminAttempts[source]
	if !attempt.BlockedUntil.After(now) {
		return 0
	}
	return attempt.BlockedUntil.Sub(now)
}

// RecordAdminLogin applies exponential backoff after three consecutive
// failures. A successful authentication clears the source state.
func (m *Manager) RecordAdminLogin(source string, success bool) {
	source = strings.TrimSpace(source)
	if source == "" {
		source = "unknown"
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if success {
		delete(m.adminAttempts, source)
		return
	}
	attempt := m.adminAttempts[source]
	attempt.Failures++
	attempt.LastAttempt = now
	if attempt.Failures >= 3 {
		exponent := min(attempt.Failures-3, 6)
		attempt.BlockedUntil = now.Add(time.Second * time.Duration(1<<exponent))
	}
	m.adminAttempts[source] = attempt
}

func (m *Manager) pruneAdminAttempts(now time.Time) {
	for source, attempt := range m.adminAttempts {
		if now.Sub(attempt.LastAttempt) > time.Hour {
			delete(m.adminAttempts, source)
		}
	}
}

func (m *Manager) CreateAdminSession(response http.ResponseWriter) (string, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", err
	}
	csrf, err := randomToken(24)
	if err != nil {
		return "", err
	}
	now := time.Now()
	m.mu.Lock()
	m.pruneAdminSessions(now)
	m.adminSessions[sha256.Sum256([]byte(token))] = adminSession{
		ExpiresAt: now.Add(adminSessionLifetime),
		CSRFToken: csrf,
	}
	secure := strings.HasPrefix(strings.ToLower(m.settings.PublicURL), "https://")
	m.mu.Unlock()
	http.SetCookie(response, &http.Cookie{
		Name:     adminCookieName,
		Value:    token,
		Path:     "/admin",
		MaxAge:   int(adminSessionLifetime.Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
	return csrf, nil
}

func (m *Manager) AdminSession(request *http.Request) (string, bool) {
	cookie, err := request.Cookie(adminCookieName)
	if err != nil {
		return "", false
	}
	now := time.Now()
	key := sha256.Sum256([]byte(cookie.Value))
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneAdminSessions(now)
	session, ok := m.adminSessions[key]
	if !ok || !session.ExpiresAt.After(now) {
		return "", false
	}
	return session.CSRFToken, true
}

func (m *Manager) ValidAdminCSRF(request *http.Request, value string) bool {
	expected, ok := m.AdminSession(request)
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(value)) == 1
}

func (m *Manager) DeleteAdminSession(response http.ResponseWriter, request *http.Request) {
	if cookie, err := request.Cookie(adminCookieName); err == nil {
		key := sha256.Sum256([]byte(cookie.Value))
		m.mu.Lock()
		delete(m.adminSessions, key)
		m.mu.Unlock()
	}
	http.SetCookie(response, &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/admin",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

func (m *Manager) pruneAdminSessions(now time.Time) {
	for key, session := range m.adminSessions {
		if !session.ExpiresAt.After(now) {
			delete(m.adminSessions, key)
		}
	}
}
