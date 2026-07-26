package security

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/access"
)

type memorySettingsStore struct {
	values map[string]string
}

func newMemorySettingsStore() *memorySettingsStore {
	return &memorySettingsStore{values: make(map[string]string)}
}

func (store *memorySettingsStore) AppSetting(_ context.Context, key string) (string, bool, error) {
	value, ok := store.values[key]
	return value, ok, nil
}

func (store *memorySettingsStore) SetAppSetting(_ context.Context, key, value string) error {
	store.values[key] = value
	return nil
}

func TestOpenModeRequiresStartupPermissionAndAdmin(t *testing.T) {
	t.Parallel()
	_, err := New(context.Background(), newMemorySettingsStore(), Config{
		Address: "127.0.0.1:7331",
		Initial: Settings{Mode: ModeOpen, PublicURL: "https://repo.example.com"},
	})
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("New() error = %v, want open-mode policy error", err)
	}

	_, err = New(context.Background(), newMemorySettingsStore(), Config{
		Address:   "0.0.0.0:7331",
		AllowOpen: true,
		Initial:   Settings{Mode: ModeOpen, PublicURL: "https://repo.example.com"},
	})
	if err != ErrAdminUnavailable {
		t.Fatalf("New() error = %v, want %v", err, ErrAdminUnavailable)
	}
}

func TestAdminSessionAndCSRF(t *testing.T) {
	t.Parallel()
	manager, err := New(context.Background(), newMemorySettingsStore(), Config{
		Address:       "127.0.0.1:7331",
		AdminUser:     "admin",
		AdminPassword: "correct horse battery staple",
		Initial:       Settings{Mode: ModeLocal},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !manager.AuthenticateAdmin("admin", "correct horse battery staple") {
		t.Fatal("AuthenticateAdmin() rejected valid credentials")
	}
	if manager.AuthenticateAdmin("admin", "wrong password") {
		t.Fatal("AuthenticateAdmin() accepted invalid credentials")
	}

	recorder := httptest.NewRecorder()
	csrf, err := manager.CreateAdminSession(recorder)
	if err != nil {
		t.Fatal(err)
	}
	response := recorder.Result()
	if len(response.Cookies()) != 1 {
		t.Fatalf("session cookies = %d, want 1", len(response.Cookies()))
	}
	request := httptest.NewRequest(http.MethodPost, "http://localhost:7331/admin/security", nil)
	request.AddCookie(response.Cookies()[0])
	if got, ok := manager.AdminSession(request); !ok || got != csrf {
		t.Fatalf("AdminSession() = %q, %v; want %q, true", got, ok, csrf)
	}
	if !manager.ValidAdminCSRF(request, csrf) || manager.ValidAdminCSRF(request, "wrong") {
		t.Fatal("administrator CSRF validation did not enforce the session token")
	}
}

func TestMiddlewareEnforcesLocalAndSharedBoundaries(t *testing.T) {
	t.Parallel()
	manager, err := New(context.Background(), newMemorySettingsStore(), Config{
		Address:       "127.0.0.1:7331",
		AllowOpen:     true,
		AdminUser:     "admin",
		AdminPassword: "correct horse battery staple",
		Initial:       Settings{Mode: ModeLocal},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := manager.Middleware(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		principal, ok := PrincipalFromContext(request.Context())
		if !ok {
			t.Error("authenticated request does not carry a principal")
		} else if manager.Mode() == ModeOpen && (principal.ID != "anonymous" || principal.Admin) {
			t.Errorf("open-mode principal = %#v, want non-admin anonymous", principal)
		} else if manager.Mode() == ModeLocal &&
			(principal.ID != "admin" || principal.Name != "Local administrator" || !principal.Admin) {
			t.Errorf("local-mode principal = %#v, want local administrator", principal)
		}
		viewer, ok := access.ViewerFromContext(request.Context())
		if !ok {
			t.Error("authenticated request does not carry an access viewer")
		} else if manager.Mode() == ModeLocal && (viewer.ID != "local:admin" || !viewer.Admin) {
			t.Errorf("local access viewer = %#v", viewer)
		} else if manager.Mode() == ModeOpen && (viewer.ID != "open:anonymous" || viewer.Admin) {
			t.Errorf("open access viewer = %#v", viewer)
		}
		response.WriteHeader(http.StatusNoContent)
	}))

	localRequest := httptest.NewRequest(http.MethodGet, "http://localhost:7331/", nil)
	localRequest.Host = "localhost:7331"
	localResponse := httptest.NewRecorder()
	handler.ServeHTTP(localResponse, localRequest)
	if localResponse.Code != http.StatusNoContent {
		t.Fatalf("local response = %d, want %d", localResponse.Code, http.StatusNoContent)
	}
	remoteRequest := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	remoteRequest.Host = "example.com:7331"
	remoteResponse := httptest.NewRecorder()
	handler.ServeHTTP(remoteResponse, remoteRequest)
	if remoteResponse.Code != http.StatusForbidden {
		t.Fatalf("remote local-mode response = %d, want %d", remoteResponse.Code, http.StatusForbidden)
	}

	err = manager.UpdateSettings(context.Background(), Settings{
		Mode:      ModeOpen,
		PublicURL: "https://repo.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	sharedRequest := httptest.NewRequest(http.MethodGet, "https://repo.example.com/", nil)
	sharedRequest.Host = "repo.example.com"
	sharedRequest.Header.Set("Origin", "https://repo.example.com")
	sharedResponse := httptest.NewRecorder()
	handler.ServeHTTP(sharedResponse, sharedRequest)
	if sharedResponse.Code != http.StatusNoContent {
		t.Fatalf("shared response = %d, want %d", sharedResponse.Code, http.StatusNoContent)
	}
	badOriginRequest := httptest.NewRequest(http.MethodGet, "https://repo.example.com/", nil)
	badOriginRequest.Host = "repo.example.com"
	badOriginRequest.Header.Set("Origin", "https://evil.example")
	badOriginResponse := httptest.NewRecorder()
	handler.ServeHTTP(badOriginResponse, badOriginRequest)
	if badOriginResponse.Code != http.StatusForbidden {
		t.Fatalf("bad-origin response = %d, want %d", badOriginResponse.Code, http.StatusForbidden)
	}
}

func TestSharedURLsCannotUseSubpaths(t *testing.T) {
	t.Parallel()
	err := validateSettings(Settings{
		Mode:      ModeOpen,
		PublicURL: "https://repo.example.com/subpath",
	}, true)
	if err == nil || !strings.Contains(err.Error(), "must not contain a path") {
		t.Fatalf("validateSettings() error = %v, want subpath error", err)
	}
}
