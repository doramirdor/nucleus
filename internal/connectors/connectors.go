// Package connectors holds the built-in manifest registry and a
// file-backed registry for user-added custom connectors.
//
// Built-in connectors carry workspace autodetect rules and metadata
// schemas; the gateway knows how to bind them to repos. Custom
// connectors are lightweight — they describe a remote MCP URL and let
// the user proxy *any* HTTP+OAuth MCP server through the gateway, at the
// cost of no autodetect / no declared metadata.
//
// Custom manifests live at ~/.nucleusmcp/connectors/<name>.toml; they're
// loaded once at startup via LoadCustom. Built-ins always win when a
// name collides.
package connectors

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/doramirdor/nucleusmcp/pkg/manifest"
	"github.com/pelletier/go-toml/v2"
)

// customMu guards custom. Loading happens once at startup; add/remove
// during CLI commands also mutates it.
var (
	customMu sync.RWMutex
	custom   = map[string]manifest.Manifest{}
)

// Get returns the manifest for a connector by name. Built-ins take
// priority over custom manifests of the same name.
func Get(name string) (manifest.Manifest, bool) {
	if m, ok := builtins[name]; ok {
		return m, true
	}
	customMu.RLock()
	defer customMu.RUnlock()
	m, ok := custom[name]
	return m, ok
}

// MustGet returns a manifest or panics.
func MustGet(name string) manifest.Manifest {
	m, ok := Get(name)
	if !ok {
		panic(fmt.Sprintf("connectors: unknown connector %q", name))
	}
	return m
}

// All returns every known manifest — built-ins and custom — sorted by name.
func All() []manifest.Manifest {
	seen := map[string]struct{}{}
	out := make([]manifest.Manifest, 0, len(builtins)+len(custom))
	for _, m := range builtins {
		seen[m.Name] = struct{}{}
		out = append(out, m)
	}
	customMu.RLock()
	for _, m := range custom {
		if _, dup := seen[m.Name]; dup {
			continue
		}
		out = append(out, m)
	}
	customMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names returns every known connector name, sorted.
func Names() []string {
	ms := All()
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

// IsBuiltin reports whether a name is a built-in (non-removable) connector.
func IsBuiltin(name string) bool {
	_, ok := builtins[name]
	return ok
}

// IsCustom reports whether a name is a user-added custom connector.
func IsCustom(name string) bool {
	customMu.RLock()
	defer customMu.RUnlock()
	_, ok := custom[name]
	return ok
}

// ── custom manifest file storage ────────────────────────────────────────

// CustomDir is where file-backed custom manifests live.
func CustomDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".nucleusmcp", "connectors"), nil
}

// customManifestFile is the TOML schema on disk. Kept narrow and stable
// so users who inspect / hand-edit it aren't surprised by internal
// fields. Only http+oauth is supported today (we spawn mcp-remote).
type customManifestFile struct {
	Description string `toml:"description,omitempty"`
	Transport   string `toml:"transport"`
	URL         string `toml:"url"`
	Auth        string `toml:"auth"`
}

// LoadCustom loads every *.toml file in CustomDir() into the in-memory
// registry. Idempotent — safe to call multiple times, though the CLI
// only needs it once at process startup.
func LoadCustom() error {
	dir, err := CustomDir()
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no custom dir yet — not an error
		}
		return fmt.Errorf("read %s: %w", dir, err)
	}

	customMu.Lock()
	defer customMu.Unlock()

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}

		var raw customManifestFile
		if err := toml.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}

		name := strings.TrimSuffix(e.Name(), ".toml")
		// Built-ins win — don't allow a custom manifest to shadow a
		// built-in connector by accident.
		if _, isBuiltin := builtins[name]; isBuiltin {
			continue
		}

		m, err := manifestFromFile(name, raw)
		if err != nil {
			return fmt.Errorf("invalid manifest %s: %w", path, err)
		}
		custom[name] = m
	}
	return nil
}

// SaveCustom persists a custom manifest to disk and registers it in-memory.
// Fails if name collides with a built-in.
func SaveCustom(m manifest.Manifest) error {
	if _, ok := builtins[m.Name]; ok {
		return fmt.Errorf("%s is a built-in connector; cannot override", m.Name)
	}
	if m.Name == "" {
		return fmt.Errorf("connector name is empty")
	}
	if err := validateNameOnDisk(m.Name); err != nil {
		return err
	}

	dir, err := CustomDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	raw := customManifestFile{
		Description: m.Description,
		Transport:   string(m.Transport),
		URL:         m.URL,
		Auth:        string(m.Auth),
	}
	data, err := toml.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	path := filepath.Join(dir, m.Name+".toml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	customMu.Lock()
	custom[m.Name] = m
	customMu.Unlock()
	return nil
}

// DeleteCustom removes a custom manifest both from disk and memory. No-op
// if the manifest doesn't exist. Built-ins cannot be deleted.
func DeleteCustom(name string) error {
	if _, ok := builtins[name]; ok {
		return fmt.Errorf("%s is a built-in; cannot delete", name)
	}
	dir, err := CustomDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name+".toml")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}

	customMu.Lock()
	delete(custom, name)
	customMu.Unlock()
	return nil
}

// NewCustomHTTP builds a synthetic manifest for an arbitrary HTTP MCP
// server. The spawn template is fixed: mcp-remote bridges stdio to HTTP
// and handles OAuth.
func NewCustomHTTP(name, url, description string) manifest.Manifest {
	desc := description
	if desc == "" {
		desc = "Custom HTTP MCP — " + url
	}
	return manifest.Manifest{
		Name:        name,
		Description: desc,
		Auth:        manifest.AuthOAuth,
		Transport:   manifest.TransportHTTP,
		URL:         url,
		Spawn: manifest.SpawnTemplate{
			Command: "npx",
			Args:    []string{"-y", "mcp-remote@latest"},
		},
	}
}

// manifestFromFile reconstructs a runnable manifest from the on-disk form.
// Currently only http+oauth is understood — the supervisor always drives
// these through mcp-remote.
func manifestFromFile(name string, raw customManifestFile) (manifest.Manifest, error) {
	transport := manifest.Transport(raw.Transport)
	if transport == "" {
		transport = manifest.TransportHTTP
	}
	if transport != manifest.TransportHTTP {
		return manifest.Manifest{}, fmt.Errorf(
			"transport %q is not supported for custom connectors (only http)",
			raw.Transport)
	}
	if raw.URL == "" {
		return manifest.Manifest{}, fmt.Errorf("url is required")
	}
	auth := manifest.AuthMode(raw.Auth)
	if auth == "" {
		auth = manifest.AuthOAuth
	}
	if auth != manifest.AuthOAuth {
		return manifest.Manifest{}, fmt.Errorf(
			"auth %q is not supported for custom connectors (only oauth)",
			raw.Auth)
	}
	return NewCustomHTTP(name, raw.URL, raw.Description), nil
}

// validateNameOnDisk ensures the connector name is filesystem-safe.
// Mirrors registry.ValidateName.
func validateNameOnDisk(name string) error {
	if name == "" {
		return fmt.Errorf("name is empty")
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_'
		if !ok {
			return fmt.Errorf("name %q has invalid char %q (allowed: a-z 0-9 - _)",
				name, r)
		}
	}
	return nil
}

// builtins is the full manifest registry for shipped connectors.
// Add a new connector here to bake it into the gateway binary.
var builtins = map[string]manifest.Manifest{
	"supabase": supabase,
	"github":   github,
}

// ── Supabase ────────────────────────────────────────────────────────────
//
// Supabase's MCP is hosted at https://mcp.supabase.com/mcp and
// authenticates via OAuth 2.1 (PKCE + dynamic client registration). The
// gateway doesn't implement OAuth itself — it spawns `mcp-remote` as a
// stdio bridge, pointed at the URL, with a per-profile config directory
// so two Supabase accounts can coexist without overwriting each other's
// tokens.
//
// Multi-project: one OAuth session gives access to all projects under
// the authorized Supabase account. To route tools at the right project,
// profile metadata holds `project_id` and most Supabase tools accept a
// `project_id` arg. Workspace autodetect from `supabase/config.toml`
// still works and selects the right profile.

var supabase = manifest.Manifest{
	Name:        "supabase",
	Description: "Supabase — Postgres database, auth, storage, edge functions",
	Auth:        manifest.AuthOAuth,
	Transport:   manifest.TransportHTTP,
	URL:         "https://mcp.supabase.com/mcp",
	Spawn: manifest.SpawnTemplate{
		Command: "npx",
		Args:    []string{"-y", "mcp-remote@latest"},
	},
	Metadata: []manifest.MetadataField{
		{
			Key:         "project_id",
			Label:       "Supabase project ref (optional, enables workspace auto-bind)",
			Description: "The project ref (e.g. abcdefghijklmno) — gateway matches this against `project_id` in the repo's supabase/config.toml",
		},
	},
	Autodetect: []manifest.AutodetectRule{
		{
			Files:      []string{"supabase/config.toml"},
			MatchField: "project_id",
			Extract:    extractSupabaseProjectID,
			Reason:     "matched project_id from supabase/config.toml",
		},
	},
}

// extractSupabaseProjectID parses project_id from supabase/config.toml.
func extractSupabaseProjectID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	var cfg struct {
		ProjectID string `toml:"project_id"`
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg.ProjectID, nil
}

// ── GitHub ──────────────────────────────────────────────────────────────
//
// GitHub's reference MCP server is distributed as an npm package and
// authenticates with a GitHub personal access token (classic or
// fine-grained) passed via env var. A single PAT is typically scoped to
// the issuing user — users running multiple personas (work / personal)
// or multiple org memberships want a profile per PAT.

var github = manifest.Manifest{
	Name:        "github",
	Description: "GitHub — repos, issues, PRs, code search, workflows",
	Auth:        manifest.AuthPAT,
	PATInstructions: "Generate a personal access token at:\n" +
		"  https://github.com/settings/tokens\n" +
		"\n" +
		"Minimum scopes for the reference MCP server:\n" +
		"  repo, read:org, read:user  (classic PAT)\n" +
		"  or equivalent fine-grained permissions",
	Spawn: manifest.SpawnTemplate{
		Command: "npx",
		Args:    []string{"-y", "@modelcontextprotocol/server-github"},
		EnvFromCreds: map[string]string{
			"GITHUB_PERSONAL_ACCESS_TOKEN": "access_token",
		},
	},
	Metadata: []manifest.MetadataField{
		{
			Key:         "github_user",
			Label:       "GitHub username or org (optional, for display)",
			Description: "Who this PAT belongs to — shown in `nucleus list`. Not used for resolution yet.",
		},
	},
}
