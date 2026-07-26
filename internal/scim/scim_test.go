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
	"time"

	"github.com/spolnik/RepoKarta/internal/identity"
	"github.com/spolnik/RepoKarta/internal/store"
)

func TestSCIMConfigurationDiscoveryAndValidation(t *testing.T) {
	if _, err := New(nil, nil, "this-is-a-long-scim-test-token"); err == nil {
		t.Fatal("nil identity store was accepted")
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if service, err := New(database, database, ""); err != nil || service != nil {
		t.Fatalf("blank token = %#v, %v", service, err)
	}
	if _, err := New(database, database, "too-short"); err == nil {
		t.Fatal("short bearer token was accepted")
	}
	service, err := New(database, database, "this-is-a-long-scim-test-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"/scim/v2/ServiceProviderConfig",
		"/scim/v2/ResourceTypes",
		"/scim/v2/Schemas",
	} {
		response := scimRequest(t, service.Handler(), http.MethodGet, target, nil, true)
		if response.Code != http.StatusOK ||
			response.Header().Get("Cache-Control") != "no-store" ||
			!bytes.Contains(response.Body.Bytes(), []byte(`"schemas"`)) {
			t.Fatalf("%s status = %d, body = %s", target, response.Code, response.Body.String())
		}
	}
	response := scimRequest(t, service.Handler(), http.MethodGet, "/scim/v2/Unknown", nil, true)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown resource status = %d", response.Code)
	}
}

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

func TestSCIMReplaceReadDeleteAndPagination(t *testing.T) {
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

	createUser := func(name string) scimUser {
		response := scimRequest(t, handler, http.MethodPost, "/scim/v2/Users", map[string]any{
			"userName": name,
			"active":   true,
			"emails":   []map[string]any{{"value": name}},
		}, true)
		if response.Code != http.StatusCreated {
			t.Fatalf("create user %q = %d: %s", name, response.Code, response.Body.String())
		}
		var user scimUser
		if err := json.Unmarshal(response.Body.Bytes(), &user); err != nil {
			t.Fatal(err)
		}
		return user
	}
	first := createUser("first@example.com")
	second := createUser("second@example.com")

	response := scimRequest(t, handler, http.MethodGet, "/scim/v2/Users/"+first.ID, nil, true)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"first@example.com"`)) {
		t.Fatalf("read user = %d: %s", response.Code, response.Body.String())
	}
	response = scimRequest(t, handler, http.MethodGet, "/scim/v2/Users?startIndex=2&count=1", nil, true)
	if response.Code != http.StatusOK ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"itemsPerPage":1`)) ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"totalResults":2`)) {
		t.Fatalf("paged users = %d: %s", response.Code, response.Body.String())
	}
	response = scimRequest(t, handler, http.MethodPut, "/scim/v2/Users/"+first.ID, map[string]any{
		"userName":    "renamed@example.com",
		"displayName": "Renamed",
		"active":      true,
		"roles":       []map[string]any{{"value": "administrator"}},
	}, true)
	if response.Code != http.StatusOK ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"renamed@example.com"`)) ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"administrator"`)) {
		t.Fatalf("replace user = %d: %s", response.Code, response.Body.String())
	}

	groupResponse := scimRequest(t, handler, http.MethodPost, "/scim/v2/Groups", map[string]any{
		"displayName": "Operators",
		"members": []map[string]any{
			{"value": first.ID},
			{"value": second.ID},
		},
	}, true)
	if groupResponse.Code != http.StatusCreated {
		t.Fatalf("create group = %d: %s", groupResponse.Code, groupResponse.Body.String())
	}
	var group scimGroup
	if err := json.Unmarshal(groupResponse.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}
	response = scimRequest(t, handler, http.MethodGet, "/scim/v2/Groups/"+group.ID, nil, true)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(first.ID)) {
		t.Fatalf("read group = %d: %s", response.Code, response.Body.String())
	}
	response = scimRequest(t, handler, http.MethodGet, `/scim/v2/Groups?filter=displayName%20eq%20%22Operators%22`, nil, true)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"totalResults":1`)) {
		t.Fatalf("filter groups = %d: %s", response.Code, response.Body.String())
	}
	response = scimRequest(t, handler, http.MethodPut, "/scim/v2/Groups/"+group.ID, map[string]any{
		"displayName": "Platform",
		"members":     []map[string]any{{"value": first.ID}},
	}, true)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"Platform"`)) {
		t.Fatalf("replace group = %d: %s", response.Code, response.Body.String())
	}
	response = scimRequest(t, handler, http.MethodDelete, "/scim/v2/Groups/"+group.ID, nil, true)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete group = %d: %s", response.Code, response.Body.String())
	}
	response = scimRequest(t, handler, http.MethodDelete, "/scim/v2/Users/"+second.ID, nil, true)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete user = %d: %s", response.Code, response.Body.String())
	}
	response = scimRequest(t, handler, http.MethodGet, "/scim/v2/Users/"+second.ID, nil, true)
	if response.Code != http.StatusOK ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"active":false`)) {
		t.Fatalf("deprovisioned user read = %d: %s", response.Code, response.Body.String())
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

func TestSCIMPatchAndFilterHelpers(t *testing.T) {
	user := identity.User{
		ID: "user-1", UserName: "old", DisplayName: "Old", Email: "old@example.com",
		ExternalID: "external", Active: true, Role: identity.RoleReader,
		CreatedAt: time.Now(), AuthProvider: "saml", AuthSubject: "subject",
	}
	patch := patchRequest{Operations: []patchOperation{
		{Op: "replace", Path: "userName", Value: json.RawMessage(`"new"`)},
		{Op: "remove", Path: "displayName"},
		{Op: "replace", Path: "emails", Value: json.RawMessage(`[{"value":"new@example.com"}]`)},
		{Op: "replace", Path: "roles", Value: json.RawMessage(`[{"value":"knowledge-maintainer"}]`)},
		{Op: "remove", Path: "active"},
	}}
	if err := applyUserPatch(&user, patch); err != nil {
		t.Fatal(err)
	}
	if user.UserName != "new" || user.DisplayName != "" || user.Email != "new@example.com" ||
		user.Role != identity.RoleMaintainer || user.Active {
		t.Fatalf("patched user = %#v", user)
	}
	for _, invalid := range []patchOperation{
		{Op: "move", Path: "active", Value: json.RawMessage(`true`)},
		{Op: "replace", Path: "active", Value: json.RawMessage(`"yes"`)},
		{Op: "replace", Path: "roles", Value: json.RawMessage(`[]`)},
		{Op: "replace", Path: "unknown", Value: json.RawMessage(`"value"`)},
	} {
		copy := user
		if err := applyUserPatch(&copy, patchRequest{Operations: []patchOperation{invalid}}); err == nil {
			t.Fatalf("invalid user patch was accepted: %#v", invalid)
		}
	}

	group := identity.Group{ID: "group-1", DisplayName: "Old", Members: []string{"one", "two"}}
	groupPatch := patchRequest{Operations: []patchOperation{
		{Op: "replace", Path: "displayName", Value: json.RawMessage(`"New"`)},
		{Op: "add", Path: "members", Value: json.RawMessage(`[{"value":"three"},{"value":"one"}]`)},
		{Op: "remove", Path: `members[value eq "two"]`, Value: json.RawMessage(`null`)},
		{Op: "replace", Path: groupExtension + ":role", Value: json.RawMessage(`"administrator"`)},
	}}
	if err := applyGroupPatch(&group, groupPatch); err != nil {
		t.Fatal(err)
	}
	if group.DisplayName != "New" || group.Role != identity.RoleAdmin ||
		len(group.Members) != 2 || group.Members[0] != "one" || group.Members[1] != "three" {
		t.Fatalf("patched group = %#v", group)
	}
	if err := applyGroupPatch(&group, patchRequest{Operations: []patchOperation{{
		Op: "replace", Path: "unknown", Value: json.RawMessage(`"value"`),
	}}}); err == nil {
		t.Fatal("unknown group patch path was accepted")
	}

	users := []identity.User{
		{ID: "1", UserName: "alice", Email: "alice@example.com", ExternalID: "a"},
		{ID: "2", UserName: "bob", Email: "bob@example.com", ExternalID: "b"},
	}
	if got := filterUsers(users, `emails.value eq "bob@example.com"`); len(got) != 1 || got[0].ID != "2" {
		t.Fatalf("filtered users = %#v", got)
	}
	groups := []identity.Group{{ID: "1", DisplayName: "Alpha"}, {ID: "2", DisplayName: "Beta"}}
	if got := filterGroups(groups, `displayName eq "Beta"`); len(got) != 1 || got[0].ID != "2" {
		t.Fatalf("filtered groups = %#v", got)
	}
	if got := slicePage(users, 0, 1); len(got) != 1 || got[0].ID != "1" {
		t.Fatalf("slice page = %#v", got)
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
