package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/audit"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/identity"
)

func TestAuditEventsAreRedactedFilteredAndRetained(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if err := database.AppendAuditEvent(ctx, audit.Event{
		ActorID: "saml:alice", Action: "generation.wiki", TargetType: "wiki",
		TargetID: "7", Outcome: "success", Provider: "saml",
		CorrelationID: "request-1", CreatedAt: time.Now().UTC().AddDate(0, 0, -2),
		Metadata: map[string]string{"token": "never-store", "model": "test"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.AppendAuditEvent(ctx, audit.Event{
		ActorID: "saml:bob", Action: "authorization.denied", TargetType: "request",
		TargetID: "/api/admin/audit", Outcome: "denied", Provider: "saml",
		CorrelationID: "request-2",
	}); err != nil {
		t.Fatal(err)
	}
	page, err := database.AuditEvents(ctx, audit.Filter{ActorID: "saml:alice", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Metadata["model"] != "test" {
		t.Fatalf("filtered audit page = %#v", page)
	}
	if _, ok := page.Events[0].Metadata["token"]; ok {
		t.Fatal("token metadata was persisted")
	}
	removed, err := database.SetAuditRetention(ctx, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed events = %d, want 1", removed)
	}
	retention, err := database.AuditRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if retention.EventCount != 1 || retention.Days != 1 || retention.MaxEvents != 100 {
		t.Fatalf("retention = %#v", retention)
	}
}

func TestRepositoryCatalogueAcquisitionAndRemovalAreAudited(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	repositoryPath := filepath.Join(t.TempDir(), "example")
	if err := database.SyncRepositories(ctx, []catalog.Repository{{
		Name: "example", Path: repositoryPath, DiscoveredAt: time.Now().UTC(),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := database.SyncRepositories(ctx, nil); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"repository.acquire", "repository.remove"} {
		page, err := database.AuditEvents(ctx, audit.Filter{Action: action, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) != 1 || page.Events[0].ActorID != "system:catalogue" {
			t.Fatalf("%s events = %#v", action, page.Events)
		}
	}
}

func TestIdentityRolesAndDeprovisioningAreImmediatelyEffective(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	user, err := database.SaveUser(ctx, identity.User{
		ExternalID: "idp-subject-7", UserName: "alice@example.com",
		DisplayName: "Alice", Email: "alice@example.com", Active: true,
		Role: identity.RoleReader, SCIMManaged: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.SaveGroup(ctx, identity.Group{
		DisplayName: "Knowledge team", Role: identity.RoleMaintainer,
		Members: []string{user.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := database.ResolveIdentity(ctx, identity.Claims{
		Provider: "saml", Subject: "idp-subject-7", Email: "alice@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Role != identity.RoleMaintainer || resolution.User.AuthProvider != "saml" {
		t.Fatalf("resolution = %#v", resolution)
	}
	if err := database.SetRoleMapping(ctx, identity.RoleMapping{
		Provider: "saml", GroupValue: "security-admins", Role: identity.RoleAdmin,
	}); err != nil {
		t.Fatal(err)
	}
	resolution, err = database.ResolveIdentity(ctx, identity.Claims{
		Provider: "saml", Subject: "idp-subject-7", Groups: []string{"security-admins"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Role != identity.RoleAdmin {
		t.Fatalf("mapped role = %q, want administrator", resolution.Role)
	}
	if err := database.DeleteUser(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	_, err = database.ResolveIdentity(ctx, identity.Claims{Provider: "saml", Subject: "idp-subject-7"})
	if !errors.Is(err, identity.ErrDeprovisioned) {
		t.Fatalf("deprovisioned resolution error = %v", err)
	}
	stored, err := database.User(ctx, user.ID)
	if err != nil || stored.Active {
		t.Fatalf("historical identity = %#v, err = %v", stored, err)
	}
}
