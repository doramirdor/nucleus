// Package supervisor manages the lifecycle of upstream MCP server children.
//
// Two transports are supported:
//
//   - TransportStdio: the upstream is a local process (e.g. an npm-based
//     MCP server). Credentials come from the vault and are injected into
//     the child env per the manifest's EnvFromCreds mapping.
//
//   - TransportHTTP: the upstream is a hosted MCP server reached over
//     HTTP. The gateway does not speak HTTP or OAuth directly; it
//     spawns `mcp-remote <URL>` as a stdio bridge with
//     MCP_REMOTE_CONFIG_DIR set to a per-profile auth dir, giving each
//     profile its own isolated OAuth session.
//
// Both paths converge on an mcp-go stdio client — the supervisor's
// protocol interactions (Initialize, ListTools, CallTool) are identical.
package supervisor

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/doramirdor/nucleusmcp/internal/registry"
	"github.com/doramirdor/nucleusmcp/internal/vault"
	"github.com/doramirdor/nucleusmcp/pkg/manifest"
)

// McpRemoteConfigEnv is the env var `mcp-remote` reads for its OAuth
// config/token directory. Exported for use in tests and the add command
// (which runs its own pre-flight OAuth handshake).
const McpRemoteConfigEnv = "MCP_REMOTE_CONFIG_DIR"

// Child is a running upstream MCP server under supervision.
type Child struct {
	Connector string
	Profile   string
	ProfileID string
	Client    *client.Client
	Tools     []mcp.Tool
}

// Supervisor owns all running children.
type Supervisor struct {
	mu       sync.Mutex
	children map[string]*Child

	clientName    string
	clientVersion string
}

// New constructs an empty supervisor.
func New(clientName, clientVersion string) *Supervisor {
	return &Supervisor{
		children:      make(map[string]*Child),
		clientName:    clientName,
		clientVersion: clientVersion,
	}
}

// SpawnProfile dispatches to the transport-specific spawner.
func (s *Supervisor) SpawnProfile(
	ctx context.Context,
	m manifest.Manifest,
	p registry.Profile,
	v vault.Backend,
) (*Child, error) {
	transport := m.Transport
	if transport == "" {
		transport = manifest.TransportStdio
	}
	switch transport {
	case manifest.TransportHTTP:
		return s.spawnHTTP(ctx, m, p, v)
	case manifest.TransportStdio:
		return s.spawnStdio(ctx, m, p, v)
	default:
		return nil, fmt.Errorf("manifest %s: unknown transport %q", m.Name, transport)
	}
}

// spawnStdio runs a local MCP child with credentials pulled from the vault
// and injected into its environment.
func (s *Supervisor) spawnStdio(
	ctx context.Context, m manifest.Manifest,
	p registry.Profile, v vault.Backend,
) (*Child, error) {
	env, err := buildStdioEnv(m, p, v)
	if err != nil {
		return nil, fmt.Errorf("build env for %s: %w", p.ID, err)
	}
	return s.finishSpawn(ctx, m, p, env, m.Spawn.Args)
}

// spawnHTTP runs mcp-remote against the manifest URL with a per-profile
// auth dir so multiple OAuth sessions stay isolated.
func (s *Supervisor) spawnHTTP(
	ctx context.Context, m manifest.Manifest,
	p registry.Profile, v vault.Backend,
) (*Child, error) {
	if m.URL == "" {
		return nil, fmt.Errorf("manifest %s: http transport requires URL", m.Name)
	}

	authDir, err := v.AuthDir(p.ID)
	if err != nil {
		return nil, fmt.Errorf("auth dir for %s: %w", p.ID, err)
	}

	env := os.Environ()
	env = append(env, McpRemoteConfigEnv+"="+authDir)
	for k, val := range m.Spawn.StaticEnv {
		env = append(env, k+"="+val)
	}

	// mcp-remote takes the MCP URL as its final positional arg.
	args := append([]string{}, m.Spawn.Args...)
	args = append(args, m.URL)

	return s.finishSpawn(ctx, m, p, env, args)
}

// finishSpawn handles the common path: start the stdio child, complete
// the MCP handshake, list tools, register.
func (s *Supervisor) finishSpawn(
	ctx context.Context, m manifest.Manifest,
	p registry.Profile, env, args []string,
) (*Child, error) {
	c, err := client.NewStdioMCPClient(m.Spawn.Command, env, args...)
	if err != nil {
		return nil, fmt.Errorf("spawn %s: %w", p.ID, err)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    s.clientName,
		Version: s.clientVersion,
	}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("initialize %s: %w", p.ID, err)
	}

	listRes, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("list tools %s: %w", p.ID, err)
	}

	child := &Child{
		Connector: p.Connector,
		Profile:   p.Name,
		ProfileID: p.ID,
		Client:    c,
		Tools:     listRes.Tools,
	}

	s.mu.Lock()
	s.children[p.ID] = child
	s.mu.Unlock()
	return child, nil
}

// Children returns a deterministic snapshot of running children.
func (s *Supervisor) Children() []*Child {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Child, 0, len(s.children))
	for _, c := range s.children {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProfileID < out[j].ProfileID })
	return out
}

// Shutdown closes all child clients.
func (s *Supervisor) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.children {
		if err := c.Client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "supervisor: close %s: %v\n", id, err)
		}
		delete(s.children, id)
	}
}

// buildStdioEnv merges parent env + manifest static env + per-profile
// credentials from the vault. Credentials override anything with the
// same key.
func buildStdioEnv(m manifest.Manifest, p registry.Profile, v vault.Backend) ([]string, error) {
	env := os.Environ()

	for k, val := range m.Spawn.StaticEnv {
		env = append(env, k+"="+val)
	}

	for envKey, credKey := range m.Spawn.EnvFromCreds {
		val, err := v.Get(p.ID, credKey)
		if err != nil {
			return nil, fmt.Errorf("credential %q missing for %s: %w",
				credKey, p.ID, err)
		}
		env = append(env, envKey+"="+val)
	}

	return env, nil
}
