// Package manifest defines the public schema for a connector manifest.
//
// A manifest describes *how* to talk to a kind of upstream MCP server:
//   - how it authenticates (PAT, OAuth2, ...)
//   - what command spawns it
//   - which credential keys map to which env vars at spawn time
//   - which metadata a profile can carry (for workspace autodetection)
//   - how to auto-detect which profile fits a workspace
//
// Manifests are pure data; they contain no secrets. Credentials live in the
// vault per profile.
package manifest

// AuthMode identifies how a connector authenticates.
type AuthMode string

const (
	// AuthPAT — user pastes a long-lived personal access token. The PAT
	// is stored in the vault and injected into the child process via env.
	AuthPAT AuthMode = "pat"

	// AuthOAuth — browser-based OAuth flow. The gateway does not handle
	// the OAuth dance itself; it spawns mcp-remote as a bridge and hands
	// it an isolated auth directory per profile.
	AuthOAuth AuthMode = "oauth"
)

// Transport is how the gateway talks to the upstream MCP server.
type Transport string

const (
	// TransportStdio (default) — the upstream is a local process we
	// spawn and talk to over its stdin/stdout.
	TransportStdio Transport = "stdio"

	// TransportHTTP — the upstream is a hosted MCP server reached over
	// HTTP. We bridge to it by spawning `mcp-remote <URL>` locally; the
	// bridge handles OAuth, PKCE, DCR, and token refresh.
	TransportHTTP Transport = "http"
)

// Manifest is the static description of a connector kind.
type Manifest struct {
	// Name is the short identifier (e.g. "supabase"). Must match the key
	// used in the connectors registry.
	Name string

	// Description is one-line human copy shown by `nucleus connectors`.
	Description string

	// Auth is how profiles of this connector authenticate.
	Auth AuthMode

	// Transport is stdio (default) or http.
	// Empty is treated as stdio for back-compat with early manifests.
	Transport Transport

	// URL is required when Transport is TransportHTTP. It is appended as
	// the last arg when spawning the mcp-remote bridge.
	URL string

	// PATInstructions is shown to the user during `nucleus add` for
	// AuthPAT connectors. Free-form; usually a URL to the dashboard page
	// where tokens are minted.
	PATInstructions string

	// Spawn describes how to run the upstream MCP server process (or the
	// mcp-remote bridge for HTTP connectors).
	Spawn SpawnTemplate

	// Metadata declares optional/required metadata fields the user can
	// attach to a profile. Used at `add` time to prompt, and at resolve
	// time to match workspace context to a profile.
	Metadata []MetadataField

	// Autodetect is the list of rules that try to pick a profile for this
	// connector based on files in the user's workspace. Rules are tried
	// in order; the first one that produces a match wins.
	Autodetect []AutodetectRule
}

// SpawnTemplate says how to launch the upstream MCP server.
type SpawnTemplate struct {
	// Command is the executable (e.g. "npx").
	Command string

	// Args are CLI args to the command. Literal — no interpolation.
	// Credentials must flow via EnvFromCreds, not Args.
	Args []string

	// EnvFromCreds maps child env var name -> vault credential key.
	EnvFromCreds map[string]string

	// StaticEnv adds fixed env vars regardless of profile.
	StaticEnv map[string]string
}

// MetadataField declares a single metadata key a profile can carry.
type MetadataField struct {
	// Key is the storage key (e.g. "project_id").
	Key string

	// Label is the prompt shown to the user at `add` time.
	Label string

	// Description is longer help text shown above the prompt.
	Description string

	// Required: if true, `add` will reject an empty answer.
	Required bool
}

// AutodetectRule is one attempt to bind a profile from workspace context.
//
// At resolve time the gateway walks from cwd upward. In each directory, if
// any of Files exists, Extract is called on that path. If Extract returns
// a non-empty value, the gateway matches that value against every profile's
// metadata[MatchField] and picks the one that matches.
type AutodetectRule struct {
	// Files are workspace-relative paths to look for. The first one found
	// in the ancestor walk is what Extract reads from.
	Files []string

	// MatchField is the metadata key on profiles to compare against.
	MatchField string

	// Extract reads the discovered file and returns the value to match.
	// Returns ("", nil) if the file exists but lacks the target key —
	// the resolver will continue to the next rule/source.
	Extract func(path string) (string, error)

	// Reason is a short human string embedded in resolver logs so users
	// understand why a profile was chosen, e.g.
	// "matched project_id from supabase/config.toml".
	Reason string
}

// CredKeys returns the deduped set of credential keys the manifest
// references via EnvFromCreds.
func (m Manifest) CredKeys() []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(m.Spawn.EnvFromCreds))
	for _, k := range m.Spawn.EnvFromCreds {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}
