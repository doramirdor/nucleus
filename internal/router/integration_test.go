// Integration tests covering the cross-cutting interactions between
// the router's surface area: ranker + fan-out + nucleus_call_plan +
// policy + audit + sticky bias. These run against fake handlers
// (no supervisor, no subprocess spawn) but exercise EVERY package
// boundary the gateway crosses on a real call: catalog ranking with
// because-explanations, fan-out plan suggestion, plan dispatch with
// the policy gate, sticky update after success, audit emission with
// the right Via/Decision/Outcome, and a follow-up find_tool that
// shows the sticky bias landed.
//
// Why not test against a real supervisor? Stdio subprocess spawn
// requires a binary on disk that speaks MCP — building one inside
// the test tree is significant work for a marginal gain in fidelity.
// The slice tested here is exactly the slice that matters for the
// "5K-stars wedge": the recommender + plan + policy + audit chain.
// Supervisor spawn/respawn is tested in its own package with stubs.

package router

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/doramirdor/nucleusmcp/internal/audit"
)

// integrationFixture wires a Router with two supabase profiles and
// two github profiles plus a real audit.Writer pointed at a temp
// file. Returns the router, the audit log path (for inspection),
// and a per-handler call counter so tests can assert "this profile
// was actually invoked."
type integrationFixture struct {
	r          *Router
	auditPath  string
	calls      *callCounter
	cleanup    func()
}

// callCounter tracks how many times each namespaced tool was called
// and (optionally) the args it saw. Concurrent-safe so fan-out tests
// don't have to invent their own.
type callCounter struct {
	mu     sync.Mutex
	counts map[string]int
	last   map[string]map[string]any
}

func newCallCounter() *callCounter {
	return &callCounter{
		counts: map[string]int{},
		last:   map[string]map[string]any{},
	}
}

func (c *callCounter) record(name string, args map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[name]++
	c.last[name] = args
}

func (c *callCounter) get(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[name]
}

// stubDispatcher implements the router.Dispatcher interface and
// records each call. Records keyed by the *namespaced* tool name so
// two stubs sharing an upstream tool name (e.g. supabase:atlas and
// supabase:default both publish execute_sql) don't collide on the
// counter. The optional replyFn lets a test simulate failure modes
// (Go error, IsError result) without piling up flag combinations.
type stubDispatcher struct {
	connector string
	alias     string
	tools     []mcp.Tool
	calls     *callCounter
	replyFn   func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)
}

func (s *stubDispatcher) CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Reconstruct the namespaced name for the counter — the proxy
	// hands us the un-namespaced upstream tool name, but tests want
	// to assert per-profile.
	key := NamespacedName(s.connector, s.alias, req.Params.Name)
	s.calls.record(key, req.GetArguments())
	if s.replyFn != nil {
		return s.replyFn(ctx, req)
	}
	return mcp.NewToolResultText("ok-from-" + key), nil
}

func newIntegrationFixture(t *testing.T) *integrationFixture {
	t.Helper()

	dir := t.TempDir()
	w, err := audit.Open(audit.Options{
		Path:     filepath.Join(dir, "audit.log"),
		FullArgs: false,
	})
	if err != nil {
		t.Fatalf("open audit: %v", err)
	}

	r := NewWithMode(mcpserver.NewMCPServer("integration", "0.0.0"), ModeSearch)
	r.SetAuditWriter(w)

	// Register profiles via the same path RegisterChild uses. Going
	// through registerDispatcher (rather than poking r.handlers
	// directly) means the policy / audit / sticky wrapper sits in
	// front of every fake handler — which is the entire point of
	// these integration tests.
	calls := newCallCounter()
	type spec struct {
		connector, alias, profileID, upstreamTool, desc string
	}
	specs := []spec{
		{"supabase", "atlas", "supabase:atlas", "execute_sql",
			"[supabase/atlas project_id=abc] Execute a SQL query against the project"},
		{"supabase", "atlas", "supabase:atlas", "list_tables",
			"[supabase/atlas project_id=abc] List tables in the database"},
		{"supabase", "default", "supabase:default", "execute_sql",
			"[supabase/default project_id=xyz] Execute a SQL query against the project"},
		{"github", "work", "github:work", "create_issue",
			"[github/work] Create a new issue in a repository"},
		{"github", "personal", "github:personal", "list_repos",
			"[github/personal] List repositories accessible to the user"},
	}
	// Group by (connector, alias, profileID) so each profile gets a
	// single dispatcher carrying its full tool list — same shape
	// supervisor.Child has.
	type key struct{ connector, alias, profileID string }
	byProfile := map[key][]mcp.Tool{}
	for _, s := range specs {
		k := key{s.connector, s.alias, s.profileID}
		byProfile[k] = append(byProfile[k], mcp.NewTool(
			s.upstreamTool,
			mcp.WithDescription(s.desc),
			mcp.WithString("query", mcp.Required()),
		))
	}
	for k, tools := range byProfile {
		d := &stubDispatcher{
			connector: k.connector,
			alias:     k.alias,
			tools:     tools,
			calls:     calls,
		}
		if err := r.registerDispatcher(d, k.connector, k.profileID, k.alias,
			ProfileContext{}, tools); err != nil {
			t.Fatalf("registerDispatcher: %v", err)
		}
	}

	return &integrationFixture{
		r:         r,
		auditPath: w.Path(),
		calls:     calls,
		cleanup:   func() { _ = w.Close() },
	}
}

// readAuditEntries loads the JSONL audit log into a slice for
// assertion. Used by every integration test that asserts on what
// got logged.
func readAuditEntries(t *testing.T, path string) []audit.Entry {
	t.Helper()
	data, err := readFile(path)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var out []audit.Entry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e audit.Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("unmarshal audit line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// TestIntegration_FindCallPlan_FullCycle is the headline test: the
// exact flow the README demos. A fan-out intent → find_tool returns
// a fanout_suggestion → the test calls call_plan with the suggested
// steps → both profiles get hit in parallel → audit records both
// invocations under ViaCallPlan with DecisionAllowed/OutcomeOK →
// the next find_tool with an ambiguous intent shows the sticky bias.
func TestIntegration_FindCallPlan_FullCycle(t *testing.T) {
	f := newIntegrationFixture(t)
	defer f.cleanup()

	// Step 1: discovery. Ask for the cross-profile comparison.
	findReq := mcp.CallToolRequest{}
	findReq.Params.Arguments = map[string]any{
		"intent": "compare execute sql between atlas and default supabase",
	}
	findRes, err := f.r.handleFindTool(context.Background(), findReq)
	if err != nil {
		t.Fatalf("find_tool: %v", err)
	}
	body := textOf(t, findRes)

	var findPayload struct {
		Tools            []findToolHit     `json:"tools"`
		FanoutSuggestion *fanoutSuggestion `json:"fanout_suggestion"`
	}
	if err := json.Unmarshal([]byte(body), &findPayload); err != nil {
		t.Fatalf("unmarshal find payload: %v\n%s", err, body)
	}
	if findPayload.FanoutSuggestion == nil {
		t.Fatalf("expected fanout suggestion: %s", body)
	}
	if len(findPayload.FanoutSuggestion.Steps) != 2 {
		t.Fatalf("want 2 fan-out steps, got %v", findPayload.FanoutSuggestion.Steps)
	}
	// Every top-N hit must carry at least one because reason —
	// audit-trail is the UX promise.
	for _, h := range findPayload.Tools {
		if len(h.Because) == 0 {
			t.Errorf("hit %q has no `because` reasons", h.Name)
		}
	}

	// Step 2: dispatch. Build a plan from the suggestion's steps.
	steps := []any{}
	for _, name := range findPayload.FanoutSuggestion.Steps {
		steps = append(steps, map[string]any{
			"name": name,
			"arguments": map[string]any{
				"query": "select count(*) from users",
			},
		})
	}
	planReq := mcp.CallToolRequest{}
	planReq.Params.Arguments = map[string]any{"steps": steps}
	planRes, err := f.r.handleCallPlan(context.Background(), planReq)
	if err != nil {
		t.Fatalf("call_plan: %v", err)
	}
	if planRes.IsError {
		t.Fatalf("plan failed: %s", textOf(t, planRes))
	}

	// Step 3: assert handlers were both invoked.
	if got := f.calls.get("supabase_atlas_execute_sql"); got != 1 {
		t.Errorf("atlas not invoked exactly once: got %d", got)
	}
	if got := f.calls.get("supabase_default_execute_sql"); got != 1 {
		t.Errorf("default not invoked exactly once: got %d", got)
	}

	// Step 4: assert audit entries. Two entries, both ViaCallPlan,
	// both DecisionAllowed/OutcomeOK, with the same args_hash
	// (identical query strings).
	entries := readAuditEntries(t, f.auditPath)
	if len(entries) != 2 {
		t.Fatalf("expected 2 audit entries, got %d: %+v", len(entries), entries)
	}
	for _, e := range entries {
		if e.Via != audit.ViaCallPlan {
			t.Errorf("entry %s: via = %s, want call_plan", e.Tool, e.Via)
		}
		if e.Decision != audit.DecisionAllowed {
			t.Errorf("entry %s: decision = %s, want allowed", e.Tool, e.Decision)
		}
		if e.Outcome != audit.OutcomeOK {
			t.Errorf("entry %s: outcome = %s, want ok", e.Tool, e.Outcome)
		}
		if !strings.HasPrefix(e.ArgsHash, "sha256:") {
			t.Errorf("entry %s: args_hash missing or malformed: %q", e.Tool, e.ArgsHash)
		}
	}
	// Identical args ⇒ identical hash.
	if entries[0].ArgsHash != entries[1].ArgsHash {
		t.Errorf("identical args produced different hashes: %q vs %q",
			entries[0].ArgsHash, entries[1].ArgsHash)
	}

	// Step 5: sticky bias. Both calls succeeded — sticky now records
	// "default" (the most recent of the two). A follow-up find_tool
	// with no alias mention should bias toward default.
	//
	// Note: we can't reliably predict which profile got the LAST
	// successful call (parallel dispatch), but exactly one of them
	// will be sticky and a sticky-only intent should put that one
	// at the top of an ambiguous query.
	follow := mcp.CallToolRequest{}
	follow.Params.Arguments = map[string]any{
		"intent": "execute sql query",
	}
	followRes, err := f.r.handleFindTool(context.Background(), follow)
	if err != nil {
		t.Fatalf("follow-up find_tool: %v", err)
	}
	var followPayload struct {
		Tools []findToolHit `json:"tools"`
	}
	if err := json.Unmarshal([]byte(textOf(t, followRes)), &followPayload); err != nil {
		t.Fatalf("follow payload: %v", err)
	}
	if len(followPayload.Tools) == 0 {
		t.Fatalf("follow-up returned no hits")
	}
	// At least one of the top-2 must carry the sticky reason —
	// proves the bias is wired through the meta-tool path, not just
	// a unit-test-only artifact of rankCatalogWithSticky.
	stuck := false
	for _, h := range followPayload.Tools[:min(2, len(followPayload.Tools))] {
		for _, why := range h.Because {
			if strings.Contains(why, "sticky") {
				stuck = true
			}
		}
	}
	if !stuck {
		t.Errorf("follow-up find_tool didn't surface sticky bias; tools=%+v",
			followPayload.Tools)
	}
}

// TestIntegration_PolicyDeniesAndAudits — wires up a deny rule and
// confirms (a) the dispatch is blocked, (b) the audit entry is
// labeled DecisionDenied/OutcomeBlocked, (c) the policy rule's
// Reason makes it into the entry's Reason field, (d) the underlying
// handler was NEVER invoked (the policy short-circuits before
// reaching the upstream — the whole point).
func TestIntegration_PolicyDeniesAndAudits(t *testing.T) {
	f := newIntegrationFixture(t)
	defer f.cleanup()

	pol, err := LoadPolicyFromBytes([]byte(`
[[rule]]
match  = "supabase:atlas"
deny   = ["execute_sql"]
reason = "atlas is PROD"
`))
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	f.r.SetPolicy(pol)

	// Direct nucleus_call → handleCall → makeHandler proxy → policy.
	callReq := mcp.CallToolRequest{}
	callReq.Params.Arguments = map[string]any{
		"name":      "supabase_atlas_execute_sql",
		"arguments": map[string]any{"query": "drop table users"},
	}
	res, err := f.r.handleCall(context.Background(), callReq)
	if err != nil {
		t.Fatalf("handleCall: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected blocked-as-error, got success: %s", textOf(t, res))
	}
	body := textOf(t, res)
	if !strings.Contains(body, "BLOCKED") || !strings.Contains(body, "atlas is PROD") {
		t.Errorf("blocked message missing context: %s", body)
	}

	// Handler must NOT have been invoked.
	if got := f.calls.get("supabase_atlas_execute_sql"); got != 0 {
		t.Errorf("policy did not block dispatch — handler invoked %d time(s)", got)
	}

	// Audit must record the denial.
	entries := readAuditEntries(t, f.auditPath)
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry (the denial), got %d", len(entries))
	}
	e := entries[0]
	if e.Decision != audit.DecisionDenied {
		t.Errorf("decision = %s, want denied", e.Decision)
	}
	if e.Outcome != audit.OutcomeBlocked {
		t.Errorf("outcome = %s, want blocked", e.Outcome)
	}
	if !strings.Contains(e.Reason, "atlas is PROD") {
		t.Errorf("reason missing rule context: %s", e.Reason)
	}
	if e.Via != audit.ViaCall {
		t.Errorf("via = %s, want nucleus_call", e.Via)
	}
}

// TestIntegration_ConfirmFlow_RetryWithPhrase — exercises the LLM-
// friendly confirm protocol end-to-end:
//
//  1. First call without phrase → blocked, reason explains exactly
//     what to send.
//  2. Second call with the EXACT phrase → succeeds.
//
// Both attempts produce audit entries. The first is DecisionConfirmRequired,
// the second is DecisionAllowed.
func TestIntegration_ConfirmFlow_RetryWithPhrase(t *testing.T) {
	f := newIntegrationFixture(t)
	defer f.cleanup()

	pol, _ := LoadPolicyFromBytes([]byte(`
[[rule]]
match   = "supabase:atlas"
confirm = ["execute_sql"]
phrase  = "I understand atlas is PRODUCTION"
`))
	f.r.SetPolicy(pol)

	// Round 1: no phrase.
	r1 := mcp.CallToolRequest{}
	r1.Params.Arguments = map[string]any{
		"name":      "supabase_atlas_execute_sql",
		"arguments": map[string]any{"query": "select count(*) from users"},
	}
	res1, err := f.r.handleCall(context.Background(), r1)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !res1.IsError {
		t.Fatalf("first call should require confirmation")
	}
	body1 := textOf(t, res1)
	if !strings.Contains(body1, "I understand atlas is PRODUCTION") {
		t.Errorf("first error must echo the required phrase verbatim: %s", body1)
	}

	// Round 2: with the phrase.
	r2 := mcp.CallToolRequest{}
	r2.Params.Arguments = map[string]any{
		"name": "supabase_atlas_execute_sql",
		"arguments": map[string]any{
			"query":      "select count(*) from users",
			"__nucleus_confirm": "I understand atlas is PRODUCTION",
		},
	}
	res2, err := f.r.handleCall(context.Background(), r2)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if res2.IsError {
		t.Fatalf("second call (with phrase) should succeed: %s", textOf(t, res2))
	}
	if got := f.calls.get("supabase_atlas_execute_sql"); got != 1 {
		t.Errorf("expected 1 successful dispatch after confirm; got %d", got)
	}

	entries := readAuditEntries(t, f.auditPath)
	if len(entries) != 2 {
		t.Fatalf("want 2 audit entries (block + allow), got %d", len(entries))
	}
	if entries[0].Decision != audit.DecisionConfirmRequired {
		t.Errorf("first entry decision = %s, want confirm-required", entries[0].Decision)
	}
	if entries[1].Decision != audit.DecisionAllowed || entries[1].Outcome != audit.OutcomeOK {
		t.Errorf("second entry should be allowed/ok, got %+v", entries[1])
	}
}

// TestIntegration_FanoutPlanRespectsPolicy — the trickiest case.
// A plan with two steps where one is denied: the denied step must
// be blocked, the other must succeed, and the merged result must
// surface both. This is the contract that makes call_plan safe to
// trust across mixed-permission profiles.
func TestIntegration_FanoutPlanRespectsPolicy(t *testing.T) {
	f := newIntegrationFixture(t)
	defer f.cleanup()

	// Deny atlas, allow default.
	pol, _ := LoadPolicyFromBytes([]byte(`
[[rule]]
match  = "supabase:atlas"
deny   = ["execute_sql"]
reason = "atlas is PROD"
`))
	f.r.SetPolicy(pol)

	plan := mcp.CallToolRequest{}
	plan.Params.Arguments = map[string]any{
		"steps": []any{
			map[string]any{
				"name":      "supabase_atlas_execute_sql",
				"arguments": map[string]any{"query": "select 1"},
			},
			map[string]any{
				"name":      "supabase_default_execute_sql",
				"arguments": map[string]any{"query": "select 1"},
			},
		},
	}
	res, err := f.r.handleCallPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("call_plan: %v", err)
	}
	body := textOf(t, res)
	var payload struct {
		Steps     int              `json:"steps"`
		Successes int              `json:"successes"`
		Failures  int              `json:"failures"`
		Results   []planStepResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if payload.Successes != 1 || payload.Failures != 1 {
		t.Errorf("expected 1 success / 1 failure split; got %+v", payload)
	}
	// atlas handler MUST NOT have been invoked.
	if got := f.calls.get("supabase_atlas_execute_sql"); got != 0 {
		t.Errorf("policy bypass: atlas handler invoked %d time(s)", got)
	}
	if got := f.calls.get("supabase_default_execute_sql"); got != 1 {
		t.Errorf("default handler invocation count = %d, want 1", got)
	}
	// Locate which step is the failure and assert its reason.
	var failed *planStepResult
	for i := range payload.Results {
		if payload.Results[i].Error != "" {
			failed = &payload.Results[i]
		}
	}
	if failed == nil {
		t.Fatalf("expected one failed step; got %+v", payload.Results)
	}
	if !strings.Contains(failed.Error, "BLOCKED") || !strings.Contains(failed.Error, "atlas is PROD") {
		t.Errorf("failed step's error should carry the policy message: %q", failed.Error)
	}

	// Audit: 2 entries. atlas → denied/blocked. default → allowed/ok.
	entries := readAuditEntries(t, f.auditPath)
	if len(entries) != 2 {
		t.Fatalf("want 2 audit entries, got %d", len(entries))
	}
	denied, allowed := 0, 0
	for _, e := range entries {
		switch e.Decision {
		case audit.DecisionDenied:
			denied++
			if e.Outcome != audit.OutcomeBlocked {
				t.Errorf("denied entry outcome = %s, want blocked", e.Outcome)
			}
		case audit.DecisionAllowed:
			allowed++
			if e.Outcome != audit.OutcomeOK {
				t.Errorf("allowed entry outcome = %s, want ok", e.Outcome)
			}
		}
		if e.Via != audit.ViaCallPlan {
			t.Errorf("via = %s, want call_plan", e.Via)
		}
	}
	if denied != 1 || allowed != 1 {
		t.Errorf("decision split wrong: denied=%d allowed=%d", denied, allowed)
	}
}

// TestIntegration_AuditFailureNeverBlocksDispatch — the contract is
// that a broken audit pipeline can never block a real call. Simulate
// a closed audit writer (writes return an error) and verify dispatch
// still succeeds and the handler still ran.
func TestIntegration_AuditFailureNeverBlocksDispatch(t *testing.T) {
	f := newIntegrationFixture(t)
	preCount := f.calls.get("supabase_atlas_execute_sql")
	// Close the writer so future writes return an error. We do NOT
	// reopen — subsequent dispatches will hit a closed writer and
	// the handler MUST still run.
	f.cleanup()

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"name":      "supabase_atlas_execute_sql",
		"arguments": map[string]any{},
	}
	res, err := f.r.handleCall(context.Background(), req)
	if err != nil {
		t.Fatalf("dispatch should not surface audit failure: %v", err)
	}
	if res.IsError {
		t.Fatalf("dispatch returned IsError; audit failure must not propagate: %s",
			textOf(t, res))
	}
	if got := f.calls.get("supabase_atlas_execute_sql"); got != preCount+1 {
		t.Errorf("handler invocation count = %d, want %d", got, preCount+1)
	}
}

// TestIntegration_DenyWithErroringInner — defense-in-depth: a deny
// rule must short-circuit before the underlying dispatcher is ever
// touched. We can't easily replace a single dispatcher post-fixture
// without leaking abstraction layers, but we *can* re-register the
// atlas profile against a guaranteed-erroring dispatcher: the deny
// rule should still win and the dispatcher must never be invoked.
//
// This locks in the "policy is checked BEFORE dispatch" ordering —
// the test would fail if a future refactor moved the policy check
// after the upstream call.
func TestIntegration_DenyWithErroringInner(t *testing.T) {
	// Build a fresh router (no fixture) so we control which
	// dispatcher is wired for atlas and can assert it was never
	// called.
	dir := t.TempDir()
	w, err := audit.Open(audit.Options{Path: filepath.Join(dir, "audit.log")})
	if err != nil {
		t.Fatalf("open audit: %v", err)
	}
	defer w.Close()

	r := NewWithMode(mcpserver.NewMCPServer("test", "0.0.0"), ModeSearch)
	r.SetAuditWriter(w)
	pol, _ := LoadPolicyFromBytes([]byte(`
[[rule]]
match = "supabase:atlas"
deny  = ["execute_sql"]
`))
	r.SetPolicy(pol)

	calls := newCallCounter()
	tool := mcp.NewTool("execute_sql",
		mcp.WithDescription("denied tool"),
		mcp.WithString("query", mcp.Required()))
	d := &stubDispatcher{
		connector: "supabase",
		alias:     "atlas",
		tools:     []mcp.Tool{tool},
		calls:     calls,
		replyFn: func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, errors.New("UNREACHABLE — policy should have blocked")
		},
	}
	if err := r.registerDispatcher(d, "supabase", "supabase:atlas", "atlas",
		ProfileContext{}, []mcp.Tool{tool}); err != nil {
		t.Fatalf("register: %v", err)
	}

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"name":      "supabase_atlas_execute_sql",
		"arguments": map[string]any{"query": "anything"},
	}
	res, err := r.handleCall(context.Background(), req)
	if err != nil {
		t.Fatalf("policy short-circuit failed; reached inner: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected blocked-as-error result")
	}
	if !strings.Contains(textOf(t, res), "BLOCKED") {
		t.Errorf("expected block message, got %s", textOf(t, res))
	}
	if got := calls.get("supabase_atlas_execute_sql"); got != 0 {
		t.Errorf("dispatcher reached despite deny — invoked %d time(s)", got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// readFile defers to os.ReadFile — wrapped so callers in this file
// stay readable without an inline os reference at every site.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
