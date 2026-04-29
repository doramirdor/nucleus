// `nucleus doctor` runs a battery of health checks and reports on each
// one with a clear PASS / WARN / FAIL marker, ending with a one-line
// "next step" suggestion when something needs attention.
//
// Why a dedicated command instead of letting `nucleus serve` print
// everything on startup? `serve` runs inside the MCP client (stdio
// frames), so its stderr ends up in the client's MCP log file, not
// the user's terminal. `doctor` is the user-facing twin: same checks,
// readable output, exit code that scripts can branch on.
//
// New first-run users will end up running this whether they hit a
// problem or not — the help text on `install` should point at it,
// and a clean PASS report from doctor is the cheap signal that says
// "everything's wired up correctly, you're not staring at a stuck
// install."
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/doramirdor/nucleusmcp/internal/audit"
	"github.com/doramirdor/nucleusmcp/internal/connectors"
	"github.com/doramirdor/nucleusmcp/internal/registry"
	"github.com/doramirdor/nucleusmcp/internal/router"
)

func newDoctorCmd() *cobra.Command {
	var (
		failOnWarn bool
		probeURL   string
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run health checks — useful for first-run setup and triage",
		Long: `Diagnoses common Nucleus configuration issues.

Checks performed (in order):
  • PATH for the 'claude' CLI (needed for stdio install)
  • PATH for 'mcp-remote' (needed for HTTP connectors)
  • Registry path is reachable + readable
  • At least one profile is registered (warning, not failure)
  • Policy file (if present) parses cleanly
  • Audit log directory is writable
  • Optional: probe a running 'nucleus serve --http' instance via --probe

Exit code is 0 on PASS, 1 on FAIL. By default WARN doesn't fail —
use --strict to flip that. Suitable for use in shell scripts and CI.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := newDiagnostics()

			d.run("claude CLI on PATH", checkClaudeCLI)
			d.run("mcp-remote on PATH (for HTTP connectors)", checkMcpRemote)
			d.run("registry reachable", checkRegistry)
			d.run("at least one profile registered", checkProfilesExist)
			d.run("policy.toml parses (if present)", checkPolicyParses)
			d.run("audit log dir writable", checkAuditDir)
			d.run("custom connectors load", checkConnectorsLoad)
			if probeURL != "" {
				d.run(fmt.Sprintf("HTTP gateway responsive at %s", probeURL),
					func() result { return probeHTTPGateway(probeURL) })
			}

			d.print()

			if d.fails > 0 {
				return fmt.Errorf("doctor: %d check(s) failed", d.fails)
			}
			if d.warns > 0 && failOnWarn {
				return fmt.Errorf("doctor: %d warning(s) (treated as failure under --strict)", d.warns)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&failOnWarn, "strict", false,
		"treat warnings as failures (exit 1 if any WARN)")
	cmd.Flags().StringVar(&probeURL, "probe", "",
		"also probe a running HTTP gateway at this URL (e.g. http://127.0.0.1:8787/mcp)")
	return cmd
}

// status is the verdict on one check. Three levels keep the output
// scannable without lying to the user about severity.
type status int

const (
	statusPass status = iota
	statusWarn
	statusFail
)

type result struct {
	status status
	// detail is the short single-line explanation shown next to the
	// PASS/WARN/FAIL marker. Keep it under ~80 chars so doctor
	// output fits on one line per check.
	detail string
	// fix is the optional one-line "how to fix" hint shown only on
	// WARN and FAIL — readers who hit a problem need a next step,
	// readers who passed don't need to scroll.
	fix string
}

type diagnostics struct {
	rows  []row
	fails int
	warns int
}

type row struct {
	name string
	res  result
}

func newDiagnostics() *diagnostics { return &diagnostics{} }

func (d *diagnostics) run(name string, fn func() result) {
	r := fn()
	d.rows = append(d.rows, row{name: name, res: r})
	switch r.status {
	case statusFail:
		d.fails++
	case statusWarn:
		d.warns++
	}
}

// print emits the report. Format: one line per check with a
// fixed-width status marker so the eye can scan the column for
// failures regardless of name length, then per-row fix hints
// underneath the rows that need attention.
func (d *diagnostics) print() {
	for _, r := range d.rows {
		fmt.Fprintf(os.Stderr, "  %s  %-40s  %s\n",
			marker(r.res.status), r.name, r.res.detail)
		if r.res.status != statusPass && r.res.fix != "" {
			fmt.Fprintf(os.Stderr, "       fix: %s\n", r.res.fix)
		}
	}
	fmt.Fprintln(os.Stderr)
	switch {
	case d.fails > 0:
		fmt.Fprintf(os.Stderr, "%d failed, %d warned. See `fix:` lines above.\n",
			d.fails, d.warns)
	case d.warns > 0:
		fmt.Fprintf(os.Stderr, "%d warning(s). Nucleus will work, but consider the suggestions above.\n",
			d.warns)
	default:
		fmt.Fprintln(os.Stderr, "All checks passed. You're good to go.")
	}
}

func marker(s status) string {
	switch s {
	case statusPass:
		return "[ ok ]"
	case statusWarn:
		return "[warn]"
	case statusFail:
		return "[fail]"
	}
	return "[????]"
}

// ---------- individual checks ----------

func checkClaudeCLI() result {
	p, err := exec.LookPath("claude")
	if err != nil {
		return result{
			status: statusWarn,
			detail: "not found on PATH — `nucleus install` will fall back to printing a config snippet",
			fix:    "install Claude Code (https://claude.com/claude-code) and re-run, or use `nucleus install --print` to install manually",
		}
	}
	return result{status: statusPass, detail: p}
}

func checkMcpRemote() result {
	p, err := exec.LookPath("mcp-remote")
	if err != nil {
		return result{
			status: statusWarn,
			detail: "not found on PATH — HTTP connectors (Supabase, Linear, etc.) will fail to spawn",
			fix:    "install via `npm install -g mcp-remote`. Stdio connectors (e.g. PAT-only ones) work without it.",
		}
	}
	return result{status: statusPass, detail: p}
}

func checkRegistry() result {
	path, err := registry.DefaultPath()
	if err != nil {
		return result{status: statusFail, detail: err.Error(),
			fix: "ensure your home directory is set ($HOME)"}
	}
	// Open without writes — Open creates if missing, which we don't
	// want from a doctor run (it'd silently materialize state).
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return result{
			status: statusWarn,
			detail: fmt.Sprintf("%s not yet created (will be created on first `nucleus add`)", path),
		}
	} else if err != nil {
		return result{status: statusFail, detail: err.Error(),
			fix: "check filesystem permissions on ~/.nucleusmcp/"}
	}
	return result{status: statusPass, detail: path}
}

func checkProfilesExist() result {
	reg, err := openRegistry()
	if err != nil {
		// Differentiate "no registry yet" (a normal first-run state)
		// from "can't open it" (a real problem). The registry check
		// just above already reports filesystem issues, so here we
		// classify all open errors as warning — the user gets a more
		// targeted error from the previous row anyway.
		return result{
			status: statusWarn,
			detail: "no registry yet — register your first profile",
			fix:    "run `nucleus add supabase` (or whichever connector you need)",
		}
	}
	defer reg.Close()
	profiles, err := reg.List()
	if err != nil {
		return result{status: statusFail, detail: err.Error(),
			fix: "registry may be corrupt; back up and remove ~/.nucleusmcp/registry.db"}
	}
	if len(profiles) == 0 {
		return result{
			status: statusWarn,
			detail: "no profiles registered yet — gateway will expose zero tools",
			fix:    "run `nucleus add <connector>` (try `nucleus connectors` for a list)",
		}
	}
	return result{
		status: statusPass,
		detail: fmt.Sprintf("%d profile(s) registered", len(profiles)),
	}
}

func checkPolicyParses() result {
	// Try the env override first, then the standard location.
	path := strings.TrimSpace(os.Getenv("NUCLEUSMCP_POLICY"))
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return result{status: statusWarn,
				detail: "could not determine home dir; skipping policy check"}
		}
		path = filepath.Join(home, ".nucleusmcp", "policy.toml")
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return result{
			status: statusPass,
			detail: "(no policy.toml; gateway runs in allow-everything mode)",
		}
	}
	pol, err := router.LoadPolicy(path)
	if err != nil {
		return result{status: statusFail, detail: err.Error(),
			fix: fmt.Sprintf("fix syntax in %s — see README §Policy for the format", path)}
	}
	if pol == nil {
		return result{status: statusPass,
			detail: fmt.Sprintf("%s: empty (no rules)", path)}
	}
	return result{
		status: statusPass,
		detail: fmt.Sprintf("%s: %d rule(s)", path, len(pol.Rules)),
	}
}

func checkAuditDir() result {
	path, err := audit.DefaultPath()
	if err != nil {
		return result{status: statusWarn, detail: err.Error()}
	}
	dir := filepath.Dir(path)
	// Probe writability by creating a temp file alongside audit.log.
	// Doing it via os.MkdirAll + os.CreateTemp catches both "dir
	// missing" and "dir unwritable" cleanly without leaving empty
	// audit.log files behind.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return result{status: statusFail, detail: err.Error(),
			fix: fmt.Sprintf("ensure %s is writable by the current user", dir)}
	}
	tmp, err := os.CreateTemp(dir, ".doctor-probe-*")
	if err != nil {
		return result{status: statusFail, detail: err.Error(),
			fix: fmt.Sprintf("ensure %s is writable by the current user", dir)}
	}
	probePath := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(probePath)
	return result{status: statusPass, detail: dir}
}

func checkConnectorsLoad() result {
	// connectors.LoadCustom is invoked unconditionally by the root
	// PersistentPreRunE, so by the time doctor runs it's either
	// succeeded or we'd never reach here. Re-running it explicitly
	// in the report makes the check visible (operators expect it
	// in the list) and surfaces any partial-load nuances in the
	// row's detail.
	if err := connectors.LoadCustom(); err != nil {
		return result{status: statusFail, detail: err.Error(),
			fix: "check ~/.nucleusmcp/connectors/*.toml for syntax errors"}
	}
	all := connectors.All()
	return result{status: statusPass,
		detail: fmt.Sprintf("%d connector manifest(s) loaded", len(all))}
}

// probeHTTPGateway sends an unauthenticated POST to the /mcp endpoint
// and reports back whether something MCP-shaped is listening. It does
// NOT speak the protocol — a 200/400/401 all indicate "something is
// there"; a connection-refused or timeout indicates "nothing is."
//
// A 401 is reported as PASS when --token is required: the gateway
// proves it's alive by rejecting an unauthenticated probe.
func probeHTTPGateway(rawURL string) result {
	rawURL = strings.TrimRight(rawURL, "/")
	if !strings.HasSuffix(rawURL, "/mcp") {
		rawURL = rawURL + "/mcp"
	}
	c := &http.Client{
		Timeout: 3 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", rawURL,
		strings.NewReader(`{"jsonrpc":"2.0","method":"initialize","id":1,"params":{}}`))
	if err != nil {
		return result{status: statusFail, detail: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := c.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return result{status: statusFail, detail: "timeout — gateway not responding",
				fix: "is `nucleus serve --http ...` running on this address?"}
		}
		// connection refused, dial failure, DNS error, etc.
		return result{status: statusFail, detail: err.Error(),
			fix: "start the gateway with `nucleus serve --http <addr>`"}
	}
	defer res.Body.Close()

	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return result{status: statusPass,
			detail: "401 — gateway up, bearer token required (good)"}
	case res.StatusCode >= 200 && res.StatusCode < 500:
		return result{status: statusPass,
			detail: fmt.Sprintf("HTTP %d — gateway up", res.StatusCode)}
	default:
		return result{status: statusFail,
			detail: fmt.Sprintf("HTTP %d — gateway responded but reports an error", res.StatusCode),
			fix:    "check the gateway logs (stderr) for what failed"}
	}
}
