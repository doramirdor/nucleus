package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/doramirdor/nucleusmcp/internal/connectors"
	"github.com/doramirdor/nucleusmcp/internal/registry"
	"github.com/doramirdor/nucleusmcp/internal/supervisor"
	"github.com/doramirdor/nucleusmcp/internal/vault"
	"github.com/doramirdor/nucleusmcp/internal/workspace"
	"github.com/doramirdor/nucleusmcp/pkg/manifest"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newAddCmd() *cobra.Command {
	var (
		profileName string
		metaFlags   []string
		transport   string
		urlFlag     string
		scope       string // accepted for Claude-parity; no-op today
	)
	cmd := &cobra.Command{
		Use:   "add <connector> [profile-name] [URL]",
		Short: "Register a new profile for a connector (or re-auth an existing one)",
		Long: `Register a new profile.

The first positional is the connector name. The others are parsed by shape:
anything containing "://" is treated as a URL; anything else is the profile
name. --url and --name flags take precedence over positionals.

Built-in connectors (supabase, github) have their URL and transport baked
in — you can still pass --transport/URL for Claude-parity muscle memory.

For any connector not known to this build, supply --transport http and a
URL; the gateway saves a custom manifest under ~/.nucleusmcp/connectors/
and proxies it via mcp-remote (OAuth + PKCE handled for you).

Examples:
  # Claude-parity syntax (long form):
  nucleus add --scope project --transport http supabase https://mcp.supabase.com/mcp

  # Shorter forms that do the same thing:
  nucleus add supabase
  nucleus add supabase work

  # Custom / unknown MCPs — bring any HTTP URL:
  nucleus add --transport http my-mcp https://example.com/mcp

  # PAT connector:
  nucleus add github personal --metadata github_user=amirdor`,
		Args: cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			connectorName := args[0]

			// --scope is accepted for Claude-parity but ignored today.
			// Everything nucleus stores is already per-user under
			// ~/.nucleusmcp/. Project-scoped profile storage (a local
			// .nucleusmcp/ in the repo) is a reasonable future feature.
			if scope != "" && scope != "user" {
				stderrf("note: --scope=%s is accepted for Claude parity but currently ignored (all state is per-user under ~/.nucleusmcp/).", scope)
			}

			// Parse positionals — anything with "://" is a URL, else a name.
			var posName, posURL string
			for _, a := range args[1:] {
				if strings.Contains(a, "://") {
					if posURL != "" {
						return fmt.Errorf("URL given more than once")
					}
					posURL = a
				} else {
					if posName != "" {
						return fmt.Errorf("profile name given more than once")
					}
					posName = a
				}
			}

			// Flags override positionals.
			requestedName := profileName
			if requestedName == "" {
				requestedName = posName
			}
			requestedURL := urlFlag
			if requestedURL == "" {
				requestedURL = posURL
			}

			m, err := resolveManifest(connectorName, requestedURL, transport)
			if err != nil {
				return err
			}

			meta, err := parseMetadataFlags(metaFlags)
			if err != nil {
				return err
			}

			return runAdd(m, requestedName, meta)
		},
	}
	cmd.Flags().StringVar(&profileName, "name", "",
		"profile name (alternative to positional arg)")
	cmd.Flags().StringSliceVar(&metaFlags, "metadata", nil,
		"metadata entry key=value (repeatable); skips the interactive prompt for that field")
	cmd.Flags().StringVar(&transport, "transport", "",
		"transport for unknown connectors (http); built-ins define their own")
	cmd.Flags().StringVar(&urlFlag, "url", "",
		"MCP URL for unknown connectors; optional for built-ins (validated against manifest)")
	cmd.Flags().StringVar(&scope, "scope", "",
		"accepted for Claude-parity (e.g. 'project', 'user'); currently a no-op")
	return cmd
}

// resolveManifest picks the manifest to use for an `add` call.
//
//   - Built-in: returns the baked-in manifest. If the user supplied a URL,
//     note that it's validated/ignored (built-in URL wins).
//   - Unknown: requires an HTTP URL. Creates, persists, and registers a
//     synthetic manifest so future `serve` runs pick it up too.
func resolveManifest(name, url, transport string) (manifest.Manifest, error) {
	if m, ok := connectors.Get(name); ok {
		if url != "" && m.URL != "" && url != m.URL {
			stderrf("note: ignoring URL %q — %s is a known connector and uses %s",
				url, name, m.URL)
		}
		if transport != "" && m.Transport != "" && transport != string(m.Transport) {
			stderrf("note: ignoring --transport=%s — built-in %s uses %s",
				transport, name, m.Transport)
		}
		return m, nil
	}

	// Unknown connector — must be an HTTP URL.
	if url == "" {
		return manifest.Manifest{}, fmt.Errorf(
			"unknown connector %q — either use a built-in (try `nucleus connectors`) "+
				"or pass a URL to register a custom one, e.g.\n"+
				"  nucleus add --transport http %s https://example.com/mcp",
			name, name)
	}
	if transport == "" {
		transport = string(manifest.TransportHTTP)
	}
	if transport != string(manifest.TransportHTTP) {
		return manifest.Manifest{}, fmt.Errorf(
			"only --transport=http is supported for custom connectors; got %q", transport)
	}

	m := connectors.NewCustomHTTP(name, url, "")
	if err := connectors.SaveCustom(m); err != nil {
		return manifest.Manifest{}, fmt.Errorf("save custom manifest: %w", err)
	}
	stderrf("✓ registered custom connector %q → %s", name, url)
	return m, nil
}

// addAction is what the user chose when a connector already has profiles.
type addAction int

const (
	actionNew    addAction = iota // create a new profile
	actionReauth                  // re-authenticate an existing one
	actionCancel                  // bail out
)

// runAdd is the entry point called by the RunE closure.
func runAdd(m manifest.Manifest, requestedName string, meta map[string]string) error {
	reg, err := openRegistry()
	if err != nil {
		return fmt.Errorf("open registry: %w", err)
	}
	defer reg.Close()

	existing, err := reg.ListByConnector(m.Name)
	if err != nil {
		return fmt.Errorf("list profiles: %w", err)
	}

	action, chosenName, err := resolveAddAction(m, existing, requestedName)
	if err != nil {
		return err
	}

	switch action {
	case actionCancel:
		stderrf("cancelled.")
		return nil
	case actionReauth:
		return runReauth(reg, m, chosenName)
	case actionNew:
		return runAddNew(reg, m, chosenName, meta)
	}
	return fmt.Errorf("unreachable add action")
}

// resolveAddAction decides what to do based on whether profiles already
// exist and whether the user asked for a specific name.
func resolveAddAction(
	m manifest.Manifest, existing []registry.Profile, requestedName string,
) (addAction, string, error) {
	// Named request: either add-new-with-that-name or collide-prompt.
	if requestedName != "" {
		if err := registry.ValidateName(requestedName); err != nil {
			return 0, "", err
		}
		for _, p := range existing {
			if p.Name == requestedName {
				return promptCollision(p)
			}
		}
		return actionNew, requestedName, nil
	}

	// No name: if nothing exists, pick "default"; otherwise show menu.
	if len(existing) == 0 {
		return actionNew, "default", nil
	}
	return promptExistingMenu(m, existing)
}

// promptCollision fires when the user passed a name that already exists.
func promptCollision(existing registry.Profile) (addAction, string, error) {
	stderrf("")
	stderrf("Profile %s already exists.", existing.ID)
	stderrf("  [r] re-authenticate this profile")
	stderrf("  [c] cancel")
	choice, err := readLine(os.Stdin, "Choice [r/c]: ")
	if err != nil {
		return 0, "", err
	}
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "r":
		return actionReauth, existing.Name, nil
	default:
		return actionCancel, "", nil
	}
}

// promptExistingMenu is the "what do you want to do" menu shown when the
// user runs `nucleus add <connector>` without a name and profiles for
// that connector already exist.
func promptExistingMenu(
	m manifest.Manifest, existing []registry.Profile,
) (addAction, string, error) {
	stderrf("")
	stderrf("Found %d existing %s profile(s):", len(existing), m.Name)
	for i, p := range existing {
		stderrf("  %d. %s   (%s)", i+1, p.ID, renderMetadata(p.Metadata))
	}
	stderrf("")
	stderrf("What would you like to do?")
	stderrf("  [n] add a new profile")
	stderrf("  [r] re-authenticate an existing one")
	stderrf("  [c] cancel")

	choice, err := readLine(os.Stdin, "Choice [n/r/c]: ")
	if err != nil {
		return 0, "", err
	}
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "n", "":
		name, err := readLine(os.Stdin, "New profile name: ")
		if err != nil {
			return 0, "", err
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return 0, "", errors.New("name is required")
		}
		if err := registry.ValidateName(name); err != nil {
			return 0, "", err
		}
		for _, p := range existing {
			if p.Name == name {
				return 0, "", fmt.Errorf("%s already exists; re-run to re-auth", p.ID)
			}
		}
		return actionNew, name, nil
	case "r":
		if len(existing) == 1 {
			return actionReauth, existing[0].Name, nil
		}
		idxStr, err := readLine(os.Stdin, "Which profile to re-authenticate? (number): ")
		if err != nil {
			return 0, "", err
		}
		idx, err := strconv.Atoi(strings.TrimSpace(idxStr))
		if err != nil || idx < 1 || idx > len(existing) {
			return 0, "", fmt.Errorf("invalid choice")
		}
		return actionReauth, existing[idx-1].Name, nil
	case "c":
		return actionCancel, "", nil
	default:
		return 0, "", fmt.Errorf("invalid choice: %q", choice)
	}
}

// ── actionNew ───────────────────────────────────────────────────────────

func runAddNew(
	reg *registry.Registry, m manifest.Manifest,
	name string, meta map[string]string,
) error {
	id := registry.MakeID(m.Name, name)

	stderrf("")
	stderrf("Adding profile: %s", id)
	stderrf("Connector: %s — %s", m.Name, m.Description)
	stderrf("")

	v := newVault()

	var creds map[string]string

	switch m.Auth {
	case manifest.AuthPAT:
		// PAT: ask for any metadata first, then token last (hidden input).
		if err := promptMetadata(m, meta); err != nil {
			return err
		}
		c, err := promptPAT(m)
		if err != nil {
			return err
		}
		creds = c

	case manifest.AuthOAuth:
		// OAuth: the browser flow is the entry point. After it completes,
		// the live MCP connection lets us query the upstream for metadata
		// options (e.g. which projects exist) and offer a picker — no
		// ref-pasting, no cd-ing into a repo.
		if err := runOAuthAndDiscover(m, v, id, meta, true); err != nil {
			_ = v.DeleteAuthDir(id)
			return fmt.Errorf("oauth handshake: %w", err)
		}
		// Any metadata field still missing after discovery falls back
		// to a normal prompt (useful when the discoverer returned
		// nothing or the user skipped the picker).
		if err := promptMetadata(m, meta); err != nil {
			return err
		}

	default:
		return fmt.Errorf("unsupported auth mode %q for %s", m.Auth, m.Name)
	}

	// Persist PAT creds in the keychain before writing the registry row;
	// if registry insert fails we roll back the creds.
	for credKey, value := range creds {
		if err := v.Set(id, credKey, value); err != nil {
			return fmt.Errorf("store credential %s: %w", credKey, err)
		}
	}

	profile := registry.Profile{
		Connector: m.Name,
		Name:      name,
		Metadata:  meta,
	}
	if err := reg.Create(profile); err != nil {
		// Roll back whatever auth state we created for this profile.
		if m.Auth == manifest.AuthPAT {
			_ = v.DeleteProfile(id, keys(creds))
		} else if m.Auth == manifest.AuthOAuth {
			_ = v.DeleteAuthDir(id)
		}
		return fmt.Errorf("save profile: %w", err)
	}

	stderrf("")
	stderrf("✓ Profile saved: %s", id)
	for _, k := range sortedKeys(meta) {
		stderrf("  %s = %s", k, meta[k])
	}
	stderrf("  Restart your MCP client (Claude, Cursor, ...) to pick up the new tools.")
	return nil
}

// ── actionReauth ────────────────────────────────────────────────────────

func runReauth(reg *registry.Registry, m manifest.Manifest, name string) error {
	id := registry.MakeID(m.Name, name)
	if _, err := reg.Get(id); err != nil {
		return fmt.Errorf("profile %s not found", id)
	}

	stderrf("")
	stderrf("Re-authenticating %s", id)
	v := newVault()

	switch m.Auth {
	case manifest.AuthPAT:
		creds, err := promptPAT(m)
		if err != nil {
			return err
		}
		for credKey, value := range creds {
			if err := v.Set(id, credKey, value); err != nil {
				return fmt.Errorf("store credential %s: %w", credKey, err)
			}
		}
		stderrf("✓ credentials updated for %s", id)

	case manifest.AuthOAuth:
		// Wipe the old auth dir so mcp-remote starts a fresh flow.
		if err := v.DeleteAuthDir(id); err != nil {
			return fmt.Errorf("clear auth dir: %w", err)
		}
		// Reauth preserves existing metadata — skip the discovery picker.
		if err := runOAuthAndDiscover(m, v, id, nil, false); err != nil {
			return fmt.Errorf("oauth handshake: %w", err)
		}
		stderrf("✓ re-authenticated %s", id)

	default:
		return fmt.Errorf("unsupported auth mode %q for %s", m.Auth, m.Name)
	}
	return nil
}

// ── OAuth pre-flight via mcp-remote ─────────────────────────────────────

// oauthHandshakeTimeout bounds how long we wait for the user to complete
// the browser flow on `add`. Five minutes covers typical SSO latency.
const oauthHandshakeTimeout = 5 * time.Minute

// runOAuthAndDiscover spawns mcp-remote, completes the OAuth handshake
// via Initialize, and — if discover is true and the connector has a
// registered discoverer — uses the live MCP client to fetch metadata
// options and present a picker. Any picked metadata is merged into
// `meta`.
//
// Pass discover=false for re-auth flows where the profile's existing
// metadata should be preserved.
func runOAuthAndDiscover(
	m manifest.Manifest, v *vault.Vault,
	profileID string, meta map[string]string, discover bool,
) error {
	if m.URL == "" {
		return fmt.Errorf("manifest %s: http transport requires URL", m.Name)
	}

	authDir, err := v.AuthDir(profileID)
	if err != nil {
		return err
	}

	stderrf("")
	stderrf("Starting OAuth flow for %s.", profileID)
	stderrf("A browser should open. If it doesn't, look for a URL in the lines below:")
	stderrf("────────────────────────────────────────────────────────────────────────")

	ctx, cancel := context.WithTimeout(context.Background(), oauthHandshakeTimeout)
	defer cancel()

	env := append(os.Environ(), supervisor.McpRemoteConfigEnv+"="+authDir)
	args := append([]string{}, m.Spawn.Args...)
	args = append(args, m.URL)

	c, err := client.NewStdioMCPClient(m.Spawn.Command, env, args...)
	if err != nil {
		return fmt.Errorf("spawn mcp-remote: %w", err)
	}
	defer c.Close()

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "nucleus-add",
		Version: version,
	}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	stderrf("────────────────────────────────────────────────────────────────────────")

	if !discover {
		return nil
	}

	// Run the per-connector discoverer, if any, and present a picker.
	if d, ok := connectors.Discoverer(m.Name); ok {
		opts, err := d(ctx, c)
		if err != nil {
			stderrf("note: could not auto-discover metadata (%v) — falling back to manual prompt", err)
		} else if len(opts) > 0 {
			picked, err := pickMetadataOption(m.Name, opts)
			if err != nil {
				return err
			}
			if picked != nil {
				for k, val := range picked.Metadata {
					if _, ok := meta[k]; !ok {
						meta[k] = val
					}
				}
			}
		}
	}

	return nil
}

// pickMetadataOption renders a numbered picker for a list of discovered
// options and returns the chosen one (or nil if the user opts out).
func pickMetadataOption(
	connectorName string, opts []connectors.MetadataOption,
) (*connectors.MetadataOption, error) {
	stderrf("")
	stderrf("Found %d %s resource(s) on this account:", len(opts), connectorName)
	for i, o := range opts {
		line := fmt.Sprintf("  %d. %s", i+1, o.Label)
		if o.Summary != "" {
			line += "   (" + o.Summary + ")"
		}
		stderrf("%s", line)
	}
	stderrf("  0. skip — I'll set metadata manually or leave it blank")
	stderrf("")

	for {
		raw, err := readLine(os.Stdin, "Pick one for this profile: ")
		if err != nil {
			return nil, err
		}
		raw = strings.TrimSpace(raw)
		if raw == "" || raw == "0" {
			return nil, nil
		}
		idx, err := strconv.Atoi(raw)
		if err != nil || idx < 1 || idx > len(opts) {
			stderrf("invalid choice — enter a number between 0 and %d", len(opts))
			continue
		}
		return &opts[idx-1], nil
	}
}

// ── PAT prompting (AuthPAT connectors) ──────────────────────────────────

func promptPAT(m manifest.Manifest) (map[string]string, error) {
	if m.PATInstructions != "" {
		stderrf("%s", m.PATInstructions)
		stderrf("")
	}
	creds := make(map[string]string, len(m.Spawn.EnvFromCreds))
	for _, credKey := range m.CredKeys() {
		val, err := readSecret(fmt.Sprintf("Paste %s: ", credKey))
		if err != nil {
			return nil, err
		}
		val = strings.TrimSpace(val)
		if val == "" {
			return nil, fmt.Errorf("%s is required", credKey)
		}
		creds[credKey] = val
	}
	return creds, nil
}

// ── metadata prompting ──────────────────────────────────────────────────

func promptMetadata(m manifest.Manifest, meta map[string]string) error {
	// Best-effort autodetect from cwd. If a manifest declares an
	// autodetect rule for a metadata field (e.g. supabase reads
	// project_id from supabase/config.toml), run that rule here so we
	// can offer the discovered value as a default at the prompt.
	cwd, _ := os.Getwd()
	detected := detectMetadataDefaults(m, cwd)

	for _, f := range m.Metadata {
		if _, ok := meta[f.Key]; ok {
			continue
		}
		if f.Description != "" {
			stderrf("%s", f.Description)
		}
		label := f.Label
		if label == "" {
			label = f.Key
		}
		if f.Required {
			label += " (required)"
		}

		defaultVal, hasDefault := detected[f.Key]
		if hasDefault {
			label += fmt.Sprintf(" [%s]", defaultVal)
		}

		val, err := readLine(os.Stdin, label+": ")
		if err != nil {
			return err
		}
		val = strings.TrimSpace(val)

		// Accept the detected value when the user presses enter.
		if val == "" && hasDefault {
			val = defaultVal
		}

		if val == "" && f.Required {
			return fmt.Errorf("metadata field %q is required", f.Key)
		}
		if val != "" {
			meta[f.Key] = val
		}
	}
	return nil
}

// detectMetadataDefaults runs each manifest autodetect rule against cwd
// and returns the extracted values keyed by their MatchField. This is
// the same machinery the resolver uses at serve time — we just run it
// at add time to pre-populate prompts.
func detectMetadataDefaults(m manifest.Manifest, cwd string) map[string]string {
	out := map[string]string{}
	if cwd == "" {
		return out
	}
	for _, rule := range m.Autodetect {
		if rule.MatchField == "" || len(rule.Files) == 0 || rule.Extract == nil {
			continue
		}
		if _, ok := out[rule.MatchField]; ok {
			continue // first-rule-wins, consistent with resolver
		}
		absPath, _, err := workspace.FindFileInAncestors(cwd, rule.Files)
		if err != nil || absPath == "" {
			continue
		}
		val, err := rule.Extract(absPath)
		if err != nil || val == "" {
			continue
		}
		out[rule.MatchField] = val
	}
	return out
}

// parseMetadataFlags turns a list of "key=value" strings into a map.
func parseMetadataFlags(flags []string) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range flags {
		i := strings.IndexByte(f, '=')
		if i <= 0 {
			return nil, fmt.Errorf("invalid --metadata %q (want key=value)", f)
		}
		out[f[:i]] = f[i+1:]
	}
	return out, nil
}

// ── io helpers ──────────────────────────────────────────────────────────

func readLine(r io.Reader, prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	br := bufio.NewReader(os.Stdin)
	line, err := br.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := keys(m)
	sort.Strings(out)
	return out
}
