package registry

import (
	"errors"
	"path/filepath"
	"testing"
)

// open returns a fresh registry rooted at a temp dir. Tests get fully
// isolated databases and no shared state.
func open(t *testing.T) *Registry {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestOpen_AppliesMigrations(t *testing.T) {
	r := open(t)

	// Confirm schema_version is at the latest migration by querying meta.
	var v string
	if err := r.db.QueryRow(
		`SELECT val FROM meta WHERE key = 'schema_version'`,
	).Scan(&v); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	want := "2" // must update if migrations is extended
	if v != want {
		t.Errorf("schema_version = %q, want %q (update test when migrations grow)", v, want)
	}
}

func TestOpen_IsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.db")
	r1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	_ = r1.Close()
	r2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	_ = r2.Close()
}

func TestValidateName(t *testing.T) {
	good := []string{"a", "acme-prod", "acme_staging", "a1", "0"}
	for _, n := range good {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
	bad := []string{"", "Acme", "with space", "colon:here", "slash/here", "dot.here"}
	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", n)
		}
	}
}

func TestCreate_Get_List(t *testing.T) {
	r := open(t)

	p := Profile{
		Connector: "supabase",
		Name:      "prod",
		Metadata:  map[string]string{"project_id": "abc"},
	}
	if err := r.Create(p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := r.Get("supabase:prod")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Connector != "supabase" || got.Name != "prod" {
		t.Errorf("Get = {%s,%s}, want {supabase,prod}", got.Connector, got.Name)
	}
	if got.Metadata["project_id"] != "abc" {
		t.Errorf("metadata.project_id = %q, want abc", got.Metadata["project_id"])
	}
	if got.IsDefault {
		t.Error("new profile should not be default")
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set automatically")
	}

	list, err := r.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v, want 1 row", list, err)
	}
}

func TestCreate_Duplicate(t *testing.T) {
	r := open(t)
	p := Profile{Connector: "supabase", Name: "prod"}
	if err := r.Create(p); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	err := r.Create(p)
	if !errors.Is(err, ErrDuplicate) {
		t.Errorf("second Create error = %v, want ErrDuplicate", err)
	}
}

func TestDelete(t *testing.T) {
	r := open(t)
	_ = r.Create(Profile{Connector: "supabase", Name: "prod"})

	if err := r.Delete("supabase:prod"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get("supabase:prod"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete error = %v, want ErrNotFound", err)
	}
	// Second delete should be ErrNotFound, not a silent success.
	if err := r.Delete("supabase:prod"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete-again error = %v, want ErrNotFound", err)
	}
}

func TestSetDefault_OnlyOnePerConnector(t *testing.T) {
	r := open(t)
	_ = r.Create(Profile{Connector: "supabase", Name: "prod"})
	_ = r.Create(Profile{Connector: "supabase", Name: "staging"})
	_ = r.Create(Profile{Connector: "github", Name: "work"})

	if err := r.SetDefault("supabase:prod"); err != nil {
		t.Fatalf("SetDefault prod: %v", err)
	}
	if d, _ := r.GetDefault("supabase"); d.Name != "prod" {
		t.Errorf("default = %q, want prod", d.Name)
	}

	// Flipping the default should clear the old one, leaving only staging.
	if err := r.SetDefault("supabase:staging"); err != nil {
		t.Fatalf("SetDefault staging: %v", err)
	}
	d, err := r.GetDefault("supabase")
	if err != nil {
		t.Fatalf("GetDefault after flip: %v", err)
	}
	if d.Name != "staging" {
		t.Errorf("default = %q, want staging", d.Name)
	}

	// Count rows that think they're default — must be exactly one.
	var n int
	if err := r.db.QueryRow(
		`SELECT COUNT(*) FROM profiles WHERE connector='supabase' AND is_default=1`,
	).Scan(&n); err != nil {
		t.Fatalf("count defaults: %v", err)
	}
	if n != 1 {
		t.Errorf("defaults-per-connector = %d, want 1", n)
	}

	// Other connector untouched.
	if _, err := r.GetDefault("github"); !errors.Is(err, ErrNotFound) {
		t.Errorf("github default should not exist, got %v", err)
	}
}

func TestListByConnector(t *testing.T) {
	r := open(t)
	_ = r.Create(Profile{Connector: "supabase", Name: "a"})
	_ = r.Create(Profile{Connector: "supabase", Name: "b"})
	_ = r.Create(Profile{Connector: "github", Name: "c"})

	supa, err := r.ListByConnector("supabase")
	if err != nil {
		t.Fatal(err)
	}
	if len(supa) != 2 {
		t.Errorf("supabase profiles = %d, want 2", len(supa))
	}
	none, _ := r.ListByConnector("never-registered")
	if len(none) != 0 {
		t.Errorf("unknown connector returned %d rows", len(none))
	}
}
