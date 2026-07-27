package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/spolnik/RepoKarta/internal/access"
	"github.com/spolnik/RepoKarta/internal/contextscope"
	_ "modernc.org/sqlite"
)

func TestMigrationFromSchema16AddsNamedContexts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repokarta.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec("PRAGMA user_version = 16"); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	rows, err := storage.db.Query(`SELECT id, selectors_json FROM named_contexts LIMIT 0`)
	if err != nil {
		t.Fatalf("named context migration: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNamedContextsSeparatePersonalDefinitionsAndProtectManagedDefaults(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "repokarta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	alice := access.WithViewer(context.Background(), access.Viewer{ID: "saml:alice"})
	bob := access.WithViewer(context.Background(), access.Viewer{ID: "saml:bob"})
	admin := access.WithViewer(context.Background(), access.Viewer{ID: "local:admin", Admin: true})
	selector := contextscope.Selector{
		Kind:         contextscope.KindRepository,
		RepositoryID: 17,
		Revision:     "0123456789012345678901234567890123456789",
	}
	personal, err := storage.CreateNamedContextRecord(alice, contextscope.NamedContextRecord{
		Title:        "Alice task",
		Category:     contextscope.CategoryPersonalTask,
		Visibility:   contextscope.VisibilityPersonal,
		DefaultScope: contextscope.DefaultPersonal,
		Selectors:    []contextscope.Selector{selector},
	})
	if err != nil {
		t.Fatal(err)
	}
	if personal.OwnerID != "saml:alice" || personal.Managed {
		t.Fatalf("personal context = %#v", personal)
	}
	if contexts, err := storage.ListNamedContextRecords(bob); err != nil || len(contexts) != 0 {
		t.Fatalf("Bob personal visibility = %#v, %v", contexts, err)
	}

	managed, err := storage.CreateNamedContextRecord(admin, contextscope.NamedContextRecord{
		Title:        "Platform fleet",
		Category:     contextscope.CategoryServiceFleet,
		Visibility:   contextscope.VisibilityShared,
		DefaultScope: contextscope.DefaultAdministrator,
		Selectors:    []contextscope.Selector{selector},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !managed.Managed {
		t.Fatalf("administrator context = %#v", managed)
	}
	bobContexts, err := storage.ListNamedContextRecords(bob)
	if err != nil || len(bobContexts) != 1 || bobContexts[0].ID != managed.ID {
		t.Fatalf("Bob shared visibility = %#v, %v", bobContexts, err)
	}
	if _, err := storage.UpdateNamedContextRecord(bob, managed.ID, managed); !errors.Is(err, contextscope.ErrNamedContextForbidden) {
		t.Fatalf("Bob update managed context error = %v", err)
	}
	if err := storage.DeleteNamedContextRecord(bob, managed.ID); !errors.Is(err, contextscope.ErrNamedContextForbidden) {
		t.Fatalf("Bob delete managed context error = %v", err)
	}
	if _, err := storage.CreateNamedContextRecord(bob, contextscope.NamedContextRecord{
		Title:        "Forbidden shared scope",
		Category:     contextscope.CategoryTeam,
		Visibility:   contextscope.VisibilityShared,
		DefaultScope: contextscope.DefaultNone,
		Selectors:    []contextscope.Selector{selector},
	}); !errors.Is(err, contextscope.ErrNamedContextForbidden) {
		t.Fatalf("Bob create shared context error = %v", err)
	}
	if _, err := storage.CreateNamedContextRecord(alice, contextscope.NamedContextRecord{
		Title:        personal.Title,
		Category:     contextscope.CategoryPersonalTask,
		Visibility:   contextscope.VisibilityPersonal,
		DefaultScope: contextscope.DefaultNone,
		Selectors:    []contextscope.Selector{selector},
	}); !errors.Is(err, contextscope.ErrNamedContextConflict) {
		t.Fatalf("duplicate title error = %v", err)
	}
}
