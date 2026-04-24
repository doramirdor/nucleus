package connectors

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/doramirdor/nucleusmcp/pkg/manifest"
)

// withIsolatedHome points HOME at a fresh tempdir for the duration of
// the test, so anything reading ~/.nucleusmcp/connectors/ reads from a
// sandbox and can't collide with the developer's real custom manifests.
// Also clears the in-memory `custom` map so one test's writes don't
// leak into the next.
func withIsolatedHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	customMu.Lock()
	custom = map[string]manifest.Manifest{}
	customMu.Unlock()
	t.Cleanup(func() {
		customMu.Lock()
		custom = map[string]manifest.Manifest{}
		customMu.Unlock()
	})
	return dir
}

func TestGet_Builtins(t *testing.T) {
	for _, name := range []string{"supabase", "github"} {
		m, ok := Get(name)
		if !ok {
			t.Errorf("Get(%q) not found; built-ins broken", name)
			continue
		}
		if m.Name != name {
			t.Errorf("Get(%q).Name = %q", name, m.Name)
		}
		if !IsBuiltin(name) {
			t.Errorf("IsBuiltin(%q) = false, want true", name)
		}
		if IsCustom(name) {
			t.Errorf("IsCustom(%q) = true, want false", name)
		}
	}
	if _, ok := Get("this-is-not-a-real-connector"); ok {
		t.Error("Get for unknown name should be (_, false)")
	}
}

func TestAll_IncludesBuiltinsAndCustomSorted(t *testing.T) {
	withIsolatedHome(t)
	if err := SaveCustom(NewCustomHTTP("linear", "https://x/mcp", "")); err != nil {
		t.Fatalf("SaveCustom: %v", err)
	}
	all := All()
	var names []string
	for _, m := range all {
		names = append(names, m.Name)
	}
	// Expect at least the built-ins + linear, sorted.
	wantPresent := []string{"github", "linear", "supabase"}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for _, w := range wantPresent {
		if !have[w] {
			t.Errorf("All() missing %q; got %v", w, names)
		}
	}
	// Sort check: output must be in ascending order.
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("All() not sorted: %q before %q", names[i-1], names[i])
			break
		}
	}
}

func TestSaveCustom_Roundtrip(t *testing.T) {
	home := withIsolatedHome(t)

	m := NewCustomHTTP("linear", "https://mcp.linear.app/mcp", "linear hosted MCP")
	if err := SaveCustom(m); err != nil {
		t.Fatalf("SaveCustom: %v", err)
	}

	// File on disk has the expected path and content shape.
	want := filepath.Join(home, ".nucleusmcp", "connectors", "linear.toml")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected manifest file at %s: %v", want, err)
	}

	// Reset in-memory then load from disk — the reload path must
	// reconstruct a manifest equivalent to what we saved.
	customMu.Lock()
	custom = map[string]manifest.Manifest{}
	customMu.Unlock()

	if err := LoadCustom(); err != nil {
		t.Fatalf("LoadCustom: %v", err)
	}
	got, ok := Get("linear")
	if !ok {
		t.Fatal("Get(linear) not found after reload")
	}
	if got.URL != m.URL || got.Transport != m.Transport || got.Auth != m.Auth {
		t.Errorf("reloaded manifest differs: got %+v want %+v", got, m)
	}
	if !IsCustom("linear") {
		t.Error("IsCustom(linear) should be true after reload")
	}
}

func TestSaveCustom_CannotShadowBuiltin(t *testing.T) {
	withIsolatedHome(t)
	err := SaveCustom(NewCustomHTTP("supabase", "https://evil.example/mcp", ""))
	if err == nil {
		t.Fatal("expected error when saving custom manifest with built-in name")
	}
}

func TestDeleteCustom(t *testing.T) {
	withIsolatedHome(t)
	_ = SaveCustom(NewCustomHTTP("linear", "https://mcp.linear.app/mcp", ""))
	if !IsCustom("linear") {
		t.Fatal("precondition: linear should be registered")
	}
	if err := DeleteCustom("linear"); err != nil {
		t.Fatalf("DeleteCustom: %v", err)
	}
	if _, ok := Get("linear"); ok {
		t.Error("Get(linear) should be (_, false) after DeleteCustom")
	}
	// Deleting a non-existent custom is a no-op.
	if err := DeleteCustom("never-existed"); err != nil {
		t.Errorf("DeleteCustom of absent name = %v, want nil", err)
	}
	// Cannot delete a built-in.
	if err := DeleteCustom("supabase"); err == nil {
		t.Error("DeleteCustom(builtin) should error")
	}
}

func TestLoadCustom_SkipsIfDirMissing(t *testing.T) {
	withIsolatedHome(t)
	// Directory does not exist at all — must not error.
	if err := LoadCustom(); err != nil {
		t.Errorf("LoadCustom with no dir = %v, want nil", err)
	}
}
