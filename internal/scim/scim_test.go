package scim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/spolnik/RepoKarta/internal/identity"
	"github.com/spolnik/RepoKarta/internal/store"
)

func TestSCIMUserLifecycleAndSessionRevocation(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service, err := New(database, database, "this-is-a-long-scim-test-token")
	if err != nil {
		t.Fatal(err)
	}
	handler := service.Handler()

	create := map[string]any{
		"schemas": []string{coreUserSchema}, "externalId": "stable-subject",
		"userName": "alice@example.com", "displayName": "Alice", "active": true,
		"emails": []map[string]any{{"value": "alice@example.com", "primary": true}},
		"roles":  []map[string]any{{"value": "knowledge-maintainer"}},
	}
	response := scimRequest(t, handler, http.MethodPost, "/scim/v2/Users", create, true)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", response.Code, response.Body.String())
	}
	var created scimUser
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || response.Header().Get("Location") == "" {
		t.Fatalf("created resource = %#v", created)
	}

	response = scimRequest(t, handler, http.MethodGet, `/scim/v2/Users?filter=userName%20eq%20%22alice@example.com%22`, nil, true)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"totalResults":1`)) {
		t.Fatalf("filter status = %d, body = %s", response.Code, response.Body.String())
	}

	patch := map[string]any{
		"schemas":    []string{patchSchema},
		"Operations": []map[string]any{{"op": "replace", "path": "active", "value": false}},
	}
	response = scimRequest(t, handler, http.MethodPatch, "/scim/v2/Users/"+created.ID, patch, true)
	if response.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", response.Code, response.Body.String())
	}
	_, err = database.ResolveIdentity(context.Background(), identity.Claims{
		Provider: "saml", Subject: "stable-subject", Email: "alice@example.com",
	})
	if !errors.Is(err, identity.ErrDeprovisioned) {
		t.Fatalf("deprovisioned authentication error = %v", err)
	}

	response = scimRequest(t, handler, http.MethodGet, "/scim/v2/Users", nil, false)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", response.Code)
	}
}

func TestSCIMGroupMembershipAndRoleExtension(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	user, err := database.SaveUser(context.Background(), identity.User{
		UserName: "member@example.com", Email: "member@example.com", Active: true,
		Role: identity.RoleReader, SCIMManaged: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, _ := New(database, database, "this-is-a-long-scim-test-token")
	group := map[string]any{
		"schemas":      []string{coreGroupSchema, groupExtension},
		"displayName":  "Maintainers",
		"members":      []map[string]any{{"value": user.ID}},
		groupExtension: map[string]any{"role": "knowledge-maintainer"},
	}
	response := scimRequest(t, service.Handler(), http.MethodPost, "/scim/v2/Groups", group, true)
	if response.Code != http.StatusCreated {
		t.Fatalf("group status = %d, body = %s", response.Code, response.Body.String())
	}
	resolution, err := database.ResolveIdentity(context.Background(), identity.Claims{
		Provider: "saml", Subject: "member-subject", Email: "member@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Role != identity.RoleMaintainer {
		t.Fatalf("group role = %q", resolution.Role)
	}
}

func scimRequest(t *testing.T, handler http.Handler, method, target string, body any, authenticated bool) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, "https://repo.example.com"+target, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/scim+json")
	if authenticated {
		request.Header.Set("Authorization", "Bearer this-is-a-long-scim-test-token")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
