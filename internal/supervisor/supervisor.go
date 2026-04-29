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
//
// # Idle reaping
//
// Each Child tracks lastUsed via every CallTool. A background reaper
// (started by StartReaper) closes children that have been idle past
// the configured timeout. The next call after reap transparently
// respawns from the captured spawn closure — the caller (router) sees
// only the wrapper Child.CallTool method, never the open/closed
// transition.
//
// Why reap rather than just leak? Each child is a forked Node or Go
// process plus a long-lived stdio pipe; a power user with a dozen
// profiles bound across several workspaces ends up with a dozen
// always-on subprocesses for hours of negligible activity. Reaping
// idle ones reclaims that footprint at the cost of a 3-5s warm-up on
// the next call — acceptable given the alternative is a permanent
// idle cost.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

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

// defaultReaperInterval is how often the reaper goroutine wakes up.
// 30s is chosen so a 15-minute idle timeout reaps within ~30s of
// crossing the threshold — fine-grained enough to free memory
// promptly, coarse enough that the goroutine itself is invisible.
const defaultReaperInterval = 30 * time.Second

// Child is a running upstream MCP server under supervision. Always
// invoke through Child.CallTool; direct access to Child.Client is
// retained for legacy callers but won't get respawn or sticky updates.
type Child struct {
	Connector string
	Profile   string
	ProfileID string
	Tools     []mcp.Tool

	// Client is the live MCP client. Nil when the child has been
	// reaped — guarded by mu and gated through CallTool, which
	// re-spawns transparently if needed.
	Client *client.Client

	mu       sync.Mutex
	lastUsed time.Time
	closed   bool
	// respawn rebuilds Client + Tools from the original spawn
	// parameters. Captured at finishSpawn time so the supervisor
	// doesn't need to remember (manifest, profile, vault) per child
	// in its own map. Nil only in tests that build a Child directly.
	respawn func(ctx context.Context) (*client.Client, []mcp.Tool, error)
}

// LastUsed returns the timestamp of the most recent successful
// CallTool. Useful for tests and admin tooling — the reaper reads it
// internally without going through this method.
func (c *Child) LastUsed() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastUsed
}

// IsClosed reports whether the child has been reaped (Client == nil).
// A closed child is still a valid Child — the next CallTool re-spawns.
func (c *Child) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// CallTool dispatches a tool call through the child's underlying
// MCP client. Transparently respawns if the child has been reaped.
// Updates lastUsed only on a successful (Go-level) round-trip — an
// upstream tool that returns IsError still counts as "the user did
// real work here," so we sticky-update either way.
func (c *Child) CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	c.mu.Lock()
	if c.closed {
		if c.respawn == nil {
			c.mu.Unlock()
			return nil, fmt.Errorf("child %s reaped and no respawn closure available", c.ProfileID)
		}
		// Respawn under the lock so two concurrent callers don't
		// each spawn a child. Cost: serialized re-init, but that
		// only matters at the moment a reaped child wakes up — the
		// steady-state CallTool path doesn't pay this.
		slog.Info("respawning idle child",
			"profile", c.ProfileID, "tool", req.Params.Name)
		cli, tools, err := c.respawn(ctx)
		if err != nil {
			c.mu.Unlock()
			return nil, fmt.Errorf("respawn %s: %w", c.ProfileID, err)
		}
		c.Client = cli
		c.Tools = tools
		c.closed = false
	}
	cli := c.Client
	c.mu.Unlock()

	res, err := cli.CallTool(ctx, req)

	c.mu.Lock()
	c.lastUsed = time.Now()
	c.mu.Unlock()

	return res, err
}

// closeForReap shuts down the underlying client and marks the child
// closed. Returns true if this call actually transitioned the child
// from open to closed; false if the child was already closed (a
// concurrent reaper got there first). The caller uses this to avoid
// double-counting / double-logging in races between concurrent
// reapers and shutdown.
func (c *Child) closeForReap() (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false, nil
	}
	c.closed = true
	if c.Client == nil {
		return true, nil
	}
	err := c.Client.Close()
	c.Client = nil
	return true, err
}

// Supervisor owns all running children.
type Supervisor struct {
	mu       sync.Mutex
	children map[string]*Child

	clientName    string
	clientVersion string

	// reaperStop closes when the user calls Shutdown (or
	// StopReaper); the reaper goroutine returns.
	reaperStop chan struct{}
	reaperOnce sync.Once
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
	v *vault.Vault,
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
	p registry.Profile, v *vault.Vault,
) (*Child, error) {
	env, err := buildStdioEnv(m, p, v)
	if err != nil {
		return nil, fmt.Errorf("build env for %s: %w", p.ID, err)
	}
	// The respawn closure captures m, p, v so an idle-reaped child
	// can rebuild itself without the supervisor having to remember
	// these values per ProfileID. Env is rebuilt each respawn (not
	// captured) so a credential rotation in the vault between reap
	// and respawn picks up the fresh value.
	respawn := func(rctx context.Context) (*client.Client, []mcp.Tool, error) {
		freshEnv, err := buildStdioEnv(m, p, v)
		if err != nil {
			return nil, nil, fmt.Errorf("rebuild env for %s: %w", p.ID, err)
		}
		return startAndInit(rctx, m, p, s.clientName, s.clientVersion, freshEnv, m.Spawn.Args)
	}
	return s.finishSpawn(ctx, m, p, env, m.Spawn.Args, respawn)
}

// spawnHTTP runs mcp-remote against the manifest URL with a per-profile
// auth dir so multiple OAuth sessions stay isolated.
func (s *Supervisor) spawnHTTP(
	ctx context.Context, m manifest.Manifest,
	p registry.Profile, v *vault.Vault,
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

	args := append([]string{}, m.Spawn.Args...)
	args = append(args, m.URL)

	respawn := func(rctx context.Context) (*client.Client, []mcp.Tool, error) {
		freshEnv := os.Environ()
		freshEnv = append(freshEnv, McpRemoteConfigEnv+"="+authDir)
		for k, val := range m.Spawn.StaticEnv {
			freshEnv = append(freshEnv, k+"="+val)
		}
		freshArgs := append([]string{}, m.Spawn.Args...)
		freshArgs = append(freshArgs, m.URL)
		return startAndInit(rctx, m, p, s.clientName, s.clientVersion, freshEnv, freshArgs)
	}

	return s.finishSpawn(ctx, m, p, env, args, respawn)
}

// finishSpawn handles the common path: start the stdio child, complete
// the MCP handshake, list tools, register.
func (s *Supervisor) finishSpawn(
	ctx context.Context, m manifest.Manifest,
	p registry.Profile, env, args []string,
	respawn func(context.Context) (*client.Client, []mcp.Tool, error),
) (*Child, error) {
	c, tools, err := startAndInit(ctx, m, p, s.clientName, s.clientVersion, env, args)
	if err != nil {
		return nil, err
	}

	child := &Child{
		Connector: p.Connector,
		Profile:   p.Name,
		ProfileID: p.ID,
		Client:    c,
		Tools:     tools,
		lastUsed:  time.Now(),
		respawn:   respawn,
	}

	s.mu.Lock()
	s.children[p.ID] = child
	s.mu.Unlock()
	return child, nil
}

// startAndInit is the shared spawn-and-handshake path used by both
// the initial spawn and the respawn closure. Pulled out so the two
// paths can't drift on the protocol-level details (initialize args,
// list-tools call).
func startAndInit(
	ctx context.Context, m manifest.Manifest, p registry.Profile,
	clientName, clientVersion string,
	env, args []string,
) (*client.Client, []mcp.Tool, error) {
	c, err := client.NewStdioMCPClient(m.Spawn.Command, env, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("spawn %s: %w", p.ID, err)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    clientName,
		Version: clientVersion,
	}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		_ = c.Close()
		return nil, nil, fmt.Errorf("initialize %s: %w", p.ID, err)
	}

	listRes, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		_ = c.Close()
		return nil, nil, fmt.Errorf("list tools %s: %w", p.ID, err)
	}
	return c, listRes.Tools, nil
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

// Shutdown closes all child clients and stops the reaper goroutine.
func (s *Supervisor) Shutdown() {
	s.StopReaper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.children {
		if _, err := c.closeForReap(); err != nil {
			fmt.Fprintf(os.Stderr, "supervisor: close %s: %v\n", id, err)
		}
		delete(s.children, id)
	}
}

// ReapIdle closes children whose lastUsed is older than `after`. The
// children themselves stay in the supervisor's map — the next
// CallTool will respawn them transparently. Returns the number reaped
// (useful for logging and tests).
func (s *Supervisor) ReapIdle(after time.Duration) int {
	if after <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-after)

	// Snapshot under s.mu to avoid holding it across the (slow)
	// stdio Close calls below.
	s.mu.Lock()
	candidates := make([]*Child, 0, len(s.children))
	for _, c := range s.children {
		c.mu.Lock()
		idle := !c.closed && c.lastUsed.Before(cutoff)
		c.mu.Unlock()
		if idle {
			candidates = append(candidates, c)
		}
	}
	s.mu.Unlock()

	reaped := 0
	for _, c := range candidates {
		transitioned, err := c.closeForReap()
		if err != nil {
			slog.Warn("reap close failed", "profile", c.ProfileID, "err", err)
			continue
		}
		// Only log/count if THIS call actually closed the child. With
		// concurrent reapers (or a Shutdown racing a tick) the same
		// child may be a candidate for multiple reapers; without the
		// transition check the logs would double-count.
		if transitioned {
			slog.Info("reaped idle child", "profile", c.ProfileID)
			reaped++
		}
	}
	return reaped
}

// StartReaper kicks off a background goroutine that periodically
// reaps children idle past `idle`. Calling twice is safe (the
// once-guard ensures only the first start has effect). Pass idle=0
// to disable; the goroutine still starts but does no work, which
// keeps the configuration shape uniform.
func (s *Supervisor) StartReaper(idle, interval time.Duration) {
	if interval <= 0 {
		interval = defaultReaperInterval
	}
	s.reaperOnce.Do(func() {
		s.reaperStop = make(chan struct{})
		go func() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-s.reaperStop:
					return
				case <-t.C:
					if idle > 0 {
						s.ReapIdle(idle)
					}
				}
			}
		}()
	})
}

// StopReaper signals the reaper goroutine to exit. Safe to call
// without StartReaper (no-op). Safe to call twice (no-op on second).
func (s *Supervisor) StopReaper() {
	if s.reaperStop == nil {
		return
	}
	select {
	case <-s.reaperStop:
		// Already closed.
	default:
		close(s.reaperStop)
	}
}

// buildStdioEnv merges parent env + manifest static env + per-profile
// credentials from the vault. Credentials override anything with the
// same key.
func buildStdioEnv(m manifest.Manifest, p registry.Profile, v *vault.Vault) ([]string, error) {
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

// errReaperStopped is returned by ReapIdle when called after
// Shutdown — indicates the supervisor isn't a useful target anymore.
// Currently unused but kept for future test/admin code that wants
// to distinguish "no idle children" from "supervisor torn down."
var errReaperStopped = errors.New("supervisor shut down")
