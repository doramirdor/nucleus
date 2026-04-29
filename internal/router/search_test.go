package router

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// makeCatalog builds a minimal catalog covering two connectors and two
// profiles each, mirroring what the real RegisterChild would produce.
func makeCatalog() []CatalogEntry {
	mk := func(connector, alias, tool, desc string) CatalogEntry {
		t := mcp.NewTool(
			NamespacedName(connector, alias, tool),
			mcp.WithDescription(desc),
			mcp.WithString("query", mcp.Required()),
		)
		return CatalogEntry{
			Name:         t.Name,
			Connector:    connector,
			Alias:        alias,
			UpstreamTool: tool,
			Tool:         t,
		}
	}
	return []CatalogEntry{
		mk("supabase", "atlas", "execute_sql",
			"[supabase/atlas project_id=abc] Execute a SQL query against the project"),
		mk("supabase", "atlas", "list_tables",
			"[supabase/atlas project_id=abc] List tables in the database"),
		mk("supabase", "default", "execute_sql",
			"[supabase/default project_id=xyz] Execute a SQL query against the project"),
		mk("github", "work", "create_issue",
			"[github/work] Create a new issue in a repository"),
		mk("github", "personal", "list_repos",
			"[github/personal] List repositories accessible to the user"),
	}
}

func TestTokenize(t *testing.T) {
	// Tokens shorter than 3 chars (`on`, `id`) are dropped by design —
	// see minTokenLen for rationale.
	got := tokenize("Execute_SQL on the supabase/atlas project_id=abc")
	want := []string{"execute", "sql", "the", "supabase", "atlas", "project", "abc"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tokenize = %v, want %v", got, want)
	}
	if len(tokenize("")) != 0 {
		t.Errorf("tokenize empty should return nil/empty")
	}
	// Bare-letter / 2-letter inputs should yield nothing useful.
	if len(tokenize("a i of on")) != 0 {
		t.Errorf("short-only input should return no tokens, got %v",
			tokenize("a i of on"))
	}
}

func TestRankCatalog_TopHitMatchesIntent(t *testing.T) {
	catalog := makeCatalog()
	hits := rankCatalog(catalog, "run a sql query on supabase atlas", "", 3)
	if len(hits) == 0 {
		t.Fatalf("expected hits, got none")
	}
	if hits[0].Name != "supabase_atlas_execute_sql" {
		t.Errorf("top hit = %q, want supabase_atlas_execute_sql; full ranking = %+v",
			hits[0].Name, hits)
	}
}

func TestRankCatalog_ConnectorFilter(t *testing.T) {
	catalog := makeCatalog()
	hits := rankCatalog(catalog, "issue", "github", 5)
	if len(hits) == 0 {
		t.Fatalf("expected at least one github hit")
	}
	for _, h := range hits {
		if h.Connector != "github" {
			t.Errorf("connector filter leaked: got hit on connector %q", h.Connector)
		}
	}
	// Top hit for "issue" + github filter should be create_issue.
	if hits[0].Name != "github_work_create_issue" {
		t.Errorf("top github hit = %q, want github_work_create_issue", hits[0].Name)
	}
}

func TestRankCatalog_LimitRespected(t *testing.T) {
	catalog := makeCatalog()
	hits := rankCatalog(catalog, "supabase", "", 2)
	if len(hits) != 2 {
		t.Errorf("limit=2 returned %d hits", len(hits))
	}
}

func TestRankCatalog_AliasBoost(t *testing.T) {
	// "atlas" is the alias on one supabase profile but not the other —
	// the alias-named profile should rank above its sibling for an
	// intent that mentions the alias.
	catalog := makeCatalog()
	hits := rankCatalog(catalog, "atlas execute sql", "", 5)
	if len(hits) < 2 {
		t.Fatalf("expected >=2 hits, got %d", len(hits))
	}
	if hits[0].Name != "supabase_atlas_execute_sql" {
		t.Errorf("alias-boosted hit ranking wrong: %+v", hits)
	}
}

func TestRankCatalog_NoMatch(t *testing.T) {
	catalog := makeCatalog()
	hits := rankCatalog(catalog, "make me a sandwich", "", 5)
	if len(hits) != 0 {
		t.Errorf("expected no hits, got %d: %+v", len(hits), hits)
	}
}

func TestRankCatalog_EmptyIntentReturnsNil(t *testing.T) {
	if rankCatalog(makeCatalog(), "", "", 5) != nil {
		t.Errorf("empty intent should return nil")
	}
}

func TestSummarizeCatalog(t *testing.T) {
	got := summarizeCatalog(makeCatalog())
	for _, want := range []string{"supabase", "github", "atlas", "work"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
	if summarizeCatalog(nil) == "" {
		t.Errorf("summarizeCatalog(nil) should not be empty")
	}
}

// TestHandleFindTool drives the meta-tool end-to-end: build a router in
// search mode, register a fake child via direct catalog injection (so we
// don't need the supervisor), call find_tool, and assert on the JSON
// payload.
func TestHandleFindTool(t *testing.T) {
	r := NewWithMode(mcpserver.NewMCPServer("test", "0.0.0"), ModeSearch)
	r.catalog = makeCatalog()

	req := mcp.CallToolRequest{}
	req.Params.Name = "nucleus_find_tool"
	req.Params.Arguments = map[string]any{
		"intent": "run a sql query",
		"limit":  float64(3),
	}
	res, err := r.handleFindTool(context.Background(), req)
	if err != nil {
		t.Fatalf("handleFindTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("find_tool returned error result: %+v", res)
	}
	body := textOf(t, res)

	var payload struct {
		Query    string        `json:"query"`
		Returned int           `json:"returned"`
		Total    int           `json:"total_in_catalog"`
		Tools    []findToolHit `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("unmarshal find_tool result: %v\nbody:\n%s", err, body)
	}
	if payload.Total != len(makeCatalog()) {
		t.Errorf("total_in_catalog = %d, want %d", payload.Total, len(makeCatalog()))
	}
	if payload.Returned == 0 {
		t.Fatalf("expected hits; got none in %s", body)
	}
	if payload.Returned > 3 {
		t.Errorf("limit not honored: returned %d", payload.Returned)
	}
	if payload.Tools[0].Name != "supabase_atlas_execute_sql" &&
		payload.Tools[0].Name != "supabase_default_execute_sql" {
		t.Errorf("top hit on 'run a sql query' should be a *_execute_sql tool, got %q",
			payload.Tools[0].Name)
	}
	// InputSchema should be a non-empty JSON object.
	if len(payload.Tools[0].InputSchema) == 0 ||
		payload.Tools[0].InputSchema[0] != '{' {
		t.Errorf("expected input_schema JSON object, got %s", payload.Tools[0].InputSchema)
	}
}

func TestHandleFindTool_RequiresIntent(t *testing.T) {
	r := NewWithMode(mcpserver.NewMCPServer("test", "0.0.0"), ModeSearch)
	r.catalog = makeCatalog()

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"intent": "  "}
	res, err := r.handleFindTool(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError on blank intent, got %+v", res)
	}
}

func TestHandleCall_UnknownToolReturnsHelpfulError(t *testing.T) {
	r := NewWithMode(mcpserver.NewMCPServer("test", "0.0.0"), ModeSearch)
	r.catalog = makeCatalog()

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"name":      "supabase_atlas_does_not_exist",
		"arguments": map[string]any{},
	}
	res, err := r.handleCall(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on unknown tool")
	}
	body := textOf(t, res)
	if !strings.Contains(body, "supabase") {
		t.Errorf("error message should mention available connectors; got: %s", body)
	}
}

func TestHandleCall_DispatchesToHandler(t *testing.T) {
	r := NewWithMode(mcpserver.NewMCPServer("test", "0.0.0"), ModeSearch)
	r.catalog = makeCatalog()

	called := false
	var sawArgs map[string]any
	r.handlers["supabase_atlas_execute_sql"] = func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		called = true
		sawArgs = req.GetArguments()
		return mcp.NewToolResultText("ok"), nil
	}

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"name":      "supabase_atlas_execute_sql",
		"arguments": map[string]any{"query": "select 1"},
	}
	res, err := r.handleCall(context.Background(), req)
	if err != nil {
		t.Fatalf("handleCall: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success result, got %+v", res)
	}
	if !called {
		t.Fatalf("inner handler not invoked")
	}
	if got, _ := sawArgs["query"].(string); got != "select 1" {
		t.Errorf("inner handler saw args %+v, want query=select 1", sawArgs)
	}
}

func TestPickCanonicalAliases_FirstAliasPerConnectorByDefault(t *testing.T) {
	r := NewWithMode(mcpserver.NewMCPServer("test", "0.0.0"), ModeHybrid)
	r.catalog = makeCatalog()

	got := r.pickCanonicalAliases()

	// First supabase alias seen is "atlas"; first github alias seen is "work".
	for _, want := range []string{"supabase:atlas", "github:work"} {
		if !got[want] {
			t.Errorf("expected canonical %q, got %v", want, got)
		}
	}
	// Non-canonical aliases must NOT be in the set.
	for _, drop := range []string{"supabase:default", "github:personal"} {
		if got[drop] {
			t.Errorf("did not expect %q in canonical set, got %v", drop, got)
		}
	}
}

func TestPickCanonicalAliases_AlwaysOnPinOverridesHeuristic(t *testing.T) {
	r := NewWithMode(mcpserver.NewMCPServer("test", "0.0.0"), ModeHybrid)
	r.catalog = makeCatalog()
	// Pin the SECOND alias for each connector — the heuristic would
	// normally pick the first.
	r.SetAlwaysOn([]string{"supabase:default", "GITHUB:Personal"}) // case-insensitive

	got := r.pickCanonicalAliases()

	for _, want := range []string{"supabase:default", "github:personal"} {
		if !got[want] {
			t.Errorf("expected pinned canonical %q, got %v", want, got)
		}
	}
	for _, drop := range []string{"supabase:atlas", "github:work"} {
		if got[drop] {
			t.Errorf("pin should have evicted %q, got %v", drop, got)
		}
	}
}

func TestPickCanonicalAliases_StaleAlwaysOnFallsBackToHeuristic(t *testing.T) {
	// A pin that doesn't match any registered profile must NOT make
	// the connector silently disappear from the canonical set — it
	// should fall back to the first-alias heuristic instead.
	r := NewWithMode(mcpserver.NewMCPServer("test", "0.0.0"), ModeHybrid)
	r.catalog = makeCatalog()
	r.SetAlwaysOn([]string{"supabase:does-not-exist"})

	got := r.pickCanonicalAliases()
	if !got["supabase:atlas"] {
		t.Errorf("stale pin should fall back to first alias 'atlas', got %v", got)
	}
}

// TestDetectFanout_KeywordPlusSiblings — the canonical case: intent
// uses "compare" and the top hit has a sibling alias offering the same
// upstream tool. The suggestion should name both supabase profiles.
func TestDetectFanout_KeywordPlusSiblings(t *testing.T) {
	catalog := makeCatalog()
	hits := rankCatalog(catalog, "compare execute sql across supabase", "", 5)
	got := detectFanout(catalog, "compare execute sql across supabase", "", hits)
	if got == nil {
		t.Fatalf("expected fanout suggestion, got nil")
	}
	if got.Connector != "supabase" || got.Tool != "execute_sql" {
		t.Errorf("wrong target: %+v", got)
	}
	wantSteps := map[string]bool{
		"supabase_atlas_execute_sql":   true,
		"supabase_default_execute_sql": true,
	}
	if len(got.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d: %v", len(got.Steps), got.Steps)
	}
	for _, s := range got.Steps {
		if !wantSteps[s] {
			t.Errorf("unexpected step %q in %v", s, got.Steps)
		}
	}
}

// TestDetectFanout_ExplicitAliasesNoKeyword — when the user names two
// aliases by name ("atlas vs default"), no comparison keyword is
// needed; the mentions themselves are enough signal.
func TestDetectFanout_ExplicitAliasesNoKeyword(t *testing.T) {
	catalog := makeCatalog()
	intent := "execute sql on atlas and default"
	hits := rankCatalog(catalog, intent, "", 5)
	got := detectFanout(catalog, intent, "", hits)
	if got == nil {
		t.Fatalf("expected fanout suggestion when 2 aliases mentioned, got nil")
	}
	if len(got.Steps) != 2 {
		t.Errorf("expected 2 steps, got %v", got.Steps)
	}
}

// TestDetectFanout_SingleProfileNoSuggestion — even with comparison
// wording, if there's only one profile for the matched tool's
// connector, fan-out is meaningless. Should return nil, not a single-
// step "plan".
func TestDetectFanout_SingleProfileNoSuggestion(t *testing.T) {
	catalog := makeCatalog()
	// "create_issue" exists only on github/work — no sibling.
	hits := rankCatalog(catalog, "compare create issue across accounts", "", 5)
	got := detectFanout(catalog, "compare create issue across accounts", "", hits)
	if got != nil {
		t.Errorf("expected nil suggestion (no siblings), got %+v", got)
	}
}

// TestDetectFanout_NoSignalNoSuggestion — a plain intent with no
// keyword and no alias mentions should not trigger fan-out, even when
// the catalog has fan-out candidates available.
func TestDetectFanout_NoSignalNoSuggestion(t *testing.T) {
	catalog := makeCatalog()
	hits := rankCatalog(catalog, "execute sql query", "", 5)
	got := detectFanout(catalog, "execute sql query", "", hits)
	if got != nil {
		t.Errorf("expected nil suggestion without trigger, got %+v", got)
	}
}

// TestHandleFindTool_EmitsFanoutSuggestion — the suggestion has to
// land in the JSON payload, not just the in-memory struct, or the LLM
// can't see it.
func TestHandleFindTool_EmitsFanoutSuggestion(t *testing.T) {
	r := NewWithMode(mcpserver.NewMCPServer("test", "0.0.0"), ModeSearch)
	r.catalog = makeCatalog()

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"intent": "compare execute sql between atlas and default",
	}
	res, err := r.handleFindTool(context.Background(), req)
	if err != nil {
		t.Fatalf("handleFindTool: %v", err)
	}
	body := textOf(t, res)
	var payload struct {
		FanoutSuggestion *fanoutSuggestion `json:"fanout_suggestion"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if payload.FanoutSuggestion == nil {
		t.Fatalf("fanout_suggestion missing from payload: %s", body)
	}
	if len(payload.FanoutSuggestion.Steps) != 2 {
		t.Errorf("want 2 fan-out steps, got %v", payload.FanoutSuggestion.Steps)
	}
}

// TestHandleCallPlan_HappyPath — two stub handlers, one plan, both
// invoked; results merged in input order.
func TestHandleCallPlan_HappyPath(t *testing.T) {
	r := NewWithMode(mcpserver.NewMCPServer("test", "0.0.0"), ModeSearch)
	r.catalog = makeCatalog()

	var mu sync.Mutex
	calls := map[string]map[string]any{}
	stub := func(name string) mcpserver.ToolHandlerFunc {
		return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			mu.Lock()
			calls[name] = req.GetArguments()
			mu.Unlock()
			return mcp.NewToolResultText("ok-" + name), nil
		}
	}
	r.handlers["supabase_atlas_execute_sql"] = stub("atlas")
	r.handlers["supabase_default_execute_sql"] = stub("default")

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"steps": []any{
			map[string]any{
				"name":      "supabase_atlas_execute_sql",
				"arguments": map[string]any{"query": "select count(*) from users"},
			},
			map[string]any{
				"name":      "supabase_default_execute_sql",
				"arguments": map[string]any{"query": "select count(*) from users"},
			},
		},
	}
	res, err := r.handleCallPlan(context.Background(), req)
	if err != nil {
		t.Fatalf("handleCallPlan: %v", err)
	}
	if res.IsError {
		t.Fatalf("plan returned error result: %s", textOf(t, res))
	}
	body := textOf(t, res)
	var payload struct {
		Steps     int              `json:"steps"`
		Successes int              `json:"successes"`
		Failures  int              `json:"failures"`
		Results   []planStepResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("unmarshal plan body: %v\n%s", err, body)
	}
	if payload.Steps != 2 || payload.Successes != 2 || payload.Failures != 0 {
		t.Errorf("plan totals wrong: %+v", payload)
	}
	if len(payload.Results) != 2 ||
		payload.Results[0].Name != "supabase_atlas_execute_sql" ||
		payload.Results[1].Name != "supabase_default_execute_sql" {
		t.Errorf("results not in input order: %+v", payload.Results)
	}
	// Both handlers should have seen the shared query.
	if got := calls["atlas"]["query"]; got != "select count(*) from users" {
		t.Errorf("atlas saw %v", got)
	}
	if got := calls["default"]["query"]; got != "select count(*) from users" {
		t.Errorf("default saw %v", got)
	}
}

// TestHandleCallPlan_PartialFailure — a step that returns an error
// must not abort the others; failures and successes coexist in the
// merged result.
func TestHandleCallPlan_PartialFailure(t *testing.T) {
	r := NewWithMode(mcpserver.NewMCPServer("test", "0.0.0"), ModeSearch)
	r.catalog = makeCatalog()

	r.handlers["supabase_atlas_execute_sql"] = func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	}
	r.handlers["supabase_default_execute_sql"] = func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, errors.New("simulated upstream blowup")
	}

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"steps": []any{
			map[string]any{"name": "supabase_atlas_execute_sql", "arguments": map[string]any{}},
			map[string]any{"name": "supabase_default_execute_sql", "arguments": map[string]any{}},
		},
	}
	res, err := r.handleCallPlan(context.Background(), req)
	if err != nil {
		t.Fatalf("handleCallPlan: %v", err)
	}
	body := textOf(t, res)
	var payload struct {
		Successes int              `json:"successes"`
		Failures  int              `json:"failures"`
		Results   []planStepResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Successes != 1 || payload.Failures != 1 {
		t.Errorf("expected 1/1 split, got %+v", payload)
	}
	// The failing step's error must be visible — that's the whole point
	// of partial-failure semantics.
	var failed *planStepResult
	for i := range payload.Results {
		if payload.Results[i].Error != "" {
			failed = &payload.Results[i]
		}
	}
	if failed == nil || !strings.Contains(failed.Error, "simulated upstream blowup") {
		t.Errorf("failed step not surfaced clearly: %+v", payload.Results)
	}
}

// TestHandleCallPlan_RejectsUnknownStep — a typo in step.name should
// fail the plan synchronously, *before* any step runs. Otherwise the
// caller might see "1 step ran, 1 failed" when actually they meant
// to invoke 2 different tools and one was a typo.
func TestHandleCallPlan_RejectsUnknownStep(t *testing.T) {
	r := NewWithMode(mcpserver.NewMCPServer("test", "0.0.0"), ModeSearch)
	r.catalog = makeCatalog()
	called := false
	r.handlers["supabase_atlas_execute_sql"] = func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		called = true
		return mcp.NewToolResultText("ok"), nil
	}

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"steps": []any{
			map[string]any{"name": "supabase_atlas_execute_sql", "arguments": map[string]any{}},
			map[string]any{"name": "supabase_typoed_tool", "arguments": map[string]any{}},
		},
	}
	res, err := r.handleCallPlan(context.Background(), req)
	if err != nil {
		t.Fatalf("handleCallPlan: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected validation error on unknown step, got success: %s", textOf(t, res))
	}
	if called {
		t.Errorf("first step ran even though plan was invalid — must reject before dispatch")
	}
}

// TestHandleCallPlan_RejectsEmptySteps — empty plan is always a
// caller mistake.
func TestHandleCallPlan_RejectsEmptySteps(t *testing.T) {
	r := NewWithMode(mcpserver.NewMCPServer("test", "0.0.0"), ModeSearch)
	r.catalog = makeCatalog()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"steps": []any{}}
	res, err := r.handleCallPlan(context.Background(), req)
	if err != nil {
		t.Fatalf("handleCallPlan: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected error on empty steps")
	}
}

// TestHandleCallPlan_ParallelismCap — when 5 slow handlers are given
// parallelism=2, no more than 2 should be in-flight simultaneously.
func TestHandleCallPlan_ParallelismCap(t *testing.T) {
	r := NewWithMode(mcpserver.NewMCPServer("test", "0.0.0"), ModeSearch)
	r.catalog = makeCatalog()

	var mu sync.Mutex
	inflight, peak := 0, 0
	slow := func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mu.Lock()
		inflight++
		if inflight > peak {
			peak = inflight
		}
		mu.Unlock()
		// 50ms is long enough that all 5 steps would overlap if the
		// scheduler weren't bounding them.
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		inflight--
		mu.Unlock()
		return mcp.NewToolResultText("ok"), nil
	}
	for _, name := range []string{
		"supabase_atlas_execute_sql",
		"supabase_atlas_list_tables",
		"supabase_default_execute_sql",
		"github_work_create_issue",
		"github_personal_list_repos",
	} {
		r.handlers[name] = slow
	}

	steps := []any{}
	for _, name := range []string{
		"supabase_atlas_execute_sql",
		"supabase_atlas_list_tables",
		"supabase_default_execute_sql",
		"github_work_create_issue",
		"github_personal_list_repos",
	} {
		steps = append(steps, map[string]any{"name": name, "arguments": map[string]any{}})
	}

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"steps":       steps,
		"parallelism": float64(2),
	}
	if _, err := r.handleCallPlan(context.Background(), req); err != nil {
		t.Fatalf("handleCallPlan: %v", err)
	}
	if peak > 2 {
		t.Errorf("parallelism=2 violated: peak in-flight = %d", peak)
	}
	if peak < 2 {
		t.Errorf("expected peak in-flight to reach 2, got %d (test sleep may be too short)", peak)
	}
}

// TestRankCatalog_BecausePopulated — every hit must carry at least
// one `because` reason, and the strings must reference the actual
// matched tokens. This is the audit-trail UX that makes the ranker
// debuggable from the LLM transcript.
func TestRankCatalog_BecausePopulated(t *testing.T) {
	catalog := makeCatalog()
	hits := rankCatalog(catalog, "execute sql atlas", "", 5)
	if len(hits) == 0 {
		t.Fatalf("expected hits")
	}
	top := hits[0]
	if len(top.Because) == 0 {
		t.Fatalf("top hit has no `because` reasons: %+v", top)
	}
	// At least one reason should mention the tool-name match — the
	// strongest signal here.
	joined := strings.Join(top.Because, " | ")
	if !strings.Contains(joined, "tool name") {
		t.Errorf("expected a tool-name reason in %q", joined)
	}
}

// TestRankCatalog_StickyBreaksTies — when two profiles of the same
// connector match identically on lexical signal, the sticky one
// wins. This is the UX that makes "I worked on staging this morning,
// keep doing that" actually feel sticky.
func TestRankCatalog_StickyBreaksTies(t *testing.T) {
	catalog := makeCatalog()
	// "execute sql" matches both supabase profiles equally — same
	// upstream tool name "execute_sql", same connector.
	without := rankCatalog(catalog, "execute sql", "", 5)
	if len(without) < 2 {
		t.Fatalf("setup: expected 2+ hits, got %d", len(without))
	}
	// Without sticky, alphabetical tie-break gives atlas first.
	if without[0].Alias != "atlas" {
		t.Fatalf("setup expectation broken: top alias = %q", without[0].Alias)
	}

	// With sticky=default, the staging profile should jump to top.
	with := rankCatalogWithSticky(catalog, "execute sql", "", 5,
		map[string]string{"supabase": "default"})
	if with[0].Alias != "default" {
		t.Errorf("sticky did not bias ranking: top alias = %q, want %q",
			with[0].Alias, "default")
	}
	// And the reason list must say so — opaque biases are bad biases.
	joined := strings.Join(with[0].Because, " | ")
	if !strings.Contains(joined, "sticky") {
		t.Errorf("top hit lacks sticky reason: %v", with[0].Because)
	}
}

// TestRankCatalog_StickySuppressedByExplicitAlias — sticky is a
// tiebreaker, not an override. If the user named "atlas" in the
// intent, sticky=default must NOT override that — the user was being
// specific.
func TestRankCatalog_StickySuppressedByExplicitAlias(t *testing.T) {
	catalog := makeCatalog()
	with := rankCatalogWithSticky(catalog, "execute sql atlas", "", 5,
		map[string]string{"supabase": "default"})
	if with[0].Alias != "atlas" {
		t.Errorf("explicit alias mention should win over sticky; got top alias %q", with[0].Alias)
	}
}

// TestMakeHandler_MarksStickyOnSuccess — makeHandler is the seam
// where every dispatch path passes through, so it owns sticky updates.
// We can't easily fake a supervisor.Child here (it holds a real MCP
// client), so this test exercises the markSticky/stickyAlias pair
// directly — the makeHandler call site is one line and trivially
// correct once those primitives work.
func TestMakeHandler_MarksStickyOnSuccess(t *testing.T) {
	r := NewWithMode(mcpserver.NewMCPServer("test", "0.0.0"), ModeSearch)
	if got := r.stickyAlias("supabase"); got != "" {
		t.Fatalf("fresh router should have no sticky, got %q", got)
	}
	r.markSticky("supabase", "atlas")
	if got := r.stickyAlias("supabase"); got != "atlas" {
		t.Errorf("sticky read-back wrong: got %q, want atlas", got)
	}
	// Case-insensitive on the connector key — connector strings tend
	// to be lower-case but we should not be brittle about that.
	if got := r.stickyAlias("Supabase"); got != "atlas" {
		t.Errorf("sticky should be case-insensitive on connector, got %q", got)
	}
	// Snapshot must be a copy — mutating the returned map should not
	// leak back into router state.
	snap := r.stickySnapshot()
	snap["supabase"] = "tampered"
	if got := r.stickyAlias("supabase"); got != "atlas" {
		t.Errorf("stickySnapshot leaked: got %q after mutating copy", got)
	}
}

// TestPolicy_DenyBlocksMatchingTool — the canonical "this profile
// can never run apply_migration" rule. Tool-name glob matches the
// upstream name and the call is blocked with a readable message.
func TestPolicy_DenyBlocksMatchingTool(t *testing.T) {
	p, err := LoadPolicyFromBytes([]byte(`
[[rule]]
match = "supabase:atlas"
deny = ["apply_*"]
reason = "atlas is PROD"
`))
	if err != nil {
		t.Fatalf("LoadPolicyFromBytes: %v", err)
	}
	d := p.Check("supabase", "atlas", "apply_migration", nil)
	if d.Allowed {
		t.Fatalf("expected deny, got allow")
	}
	if !strings.Contains(d.Message, "BLOCKED") || !strings.Contains(d.Message, "atlas is PROD") {
		t.Errorf("deny message missing context: %q", d.Message)
	}
}

// TestPolicy_DenyMissesNonMatchingProfile — a rule scoped to atlas
// must NOT block the same tool on a different profile. Exact-profile
// scoping is the whole point.
func TestPolicy_DenyMissesNonMatchingProfile(t *testing.T) {
	p, _ := LoadPolicyFromBytes([]byte(`
[[rule]]
match = "supabase:atlas"
deny = ["apply_*"]
`))
	if !p.Check("supabase", "default", "apply_migration", nil).Allowed {
		t.Errorf("rule for atlas leaked to default")
	}
}

// TestPolicy_ConfirmRequiresPhrase — confirm-mode lets the call
// through only with the magic phrase. First call (no phrase) gets a
// helpful "include this exact string" message; second call with the
// phrase passes.
func TestPolicy_ConfirmRequiresPhrase(t *testing.T) {
	p, _ := LoadPolicyFromBytes([]byte(`
[[rule]]
match = "supabase:atlas"
confirm = ["execute_sql"]
phrase = "I understand atlas is PRODUCTION"
`))
	// No phrase → blocked with instructions.
	d := p.Check("supabase", "atlas", "execute_sql", map[string]any{"query": "select 1"})
	if d.Allowed {
		t.Fatalf("expected confirmation required, got allow")
	}
	if !strings.Contains(d.Message, "I understand atlas is PRODUCTION") {
		t.Errorf("confirmation message must echo the required phrase: %q", d.Message)
	}
	// Wrong phrase → MISMATCH message (more specific than missing).
	d = p.Check("supabase", "atlas", "execute_sql", map[string]any{
		"query":      "select 1",
		confirmKey:   "yes please",
	})
	if d.Allowed || !strings.Contains(d.Message, "MISMATCH") {
		t.Errorf("expected mismatch error, got %+v", d)
	}
	// Right phrase → allowed.
	d = p.Check("supabase", "atlas", "execute_sql", map[string]any{
		"query":    "select 1",
		confirmKey: "I understand atlas is PRODUCTION",
	})
	if !d.Allowed {
		t.Errorf("correct phrase should pass, got %+v", d)
	}
}

// TestPolicy_DenyWinsOverConfirm — deny is the stricter rule; if a
// tool matches both deny and confirm patterns, deny short-circuits
// even when the right phrase is present. Otherwise a confirmed
// caller could bypass an explicit deny.
func TestPolicy_DenyWinsOverConfirm(t *testing.T) {
	p, _ := LoadPolicyFromBytes([]byte(`
[[rule]]
match = "supabase:atlas"
deny = ["execute_sql"]
confirm = ["execute_sql"]
phrase = "ok"
`))
	d := p.Check("supabase", "atlas", "execute_sql", map[string]any{
		confirmKey: "ok",
	})
	if d.Allowed {
		t.Fatalf("deny must win over confirm even with right phrase")
	}
	if !strings.Contains(d.Message, "BLOCKED") {
		t.Errorf("expected deny-shaped message, got %q", d.Message)
	}
}

// TestPolicy_WildcardProfileMatch — `supabase:*` should match any
// alias under supabase.
func TestPolicy_WildcardProfileMatch(t *testing.T) {
	p, _ := LoadPolicyFromBytes([]byte(`
[[rule]]
match = "supabase:*"
deny = ["delete_*"]
`))
	for _, alias := range []string{"atlas", "default", "anything"} {
		if p.Check("supabase", alias, "delete_branch", nil).Allowed {
			t.Errorf("wildcard match should block delete_branch on supabase:%s", alias)
		}
	}
	if !p.Check("github", "work", "delete_branch", nil).Allowed {
		t.Errorf("wildcard on supabase should not affect github")
	}
}

// TestPolicy_NilAndEmptyAreNoOps — nil policy and empty policy file
// must both behave as "allow everything." The router accepts both
// shapes; tests pin the contract so a refactor doesn't quietly
// change to "deny by default."
func TestPolicy_NilAndEmptyAreNoOps(t *testing.T) {
	var nilP *Policy
	if !nilP.Check("supabase", "atlas", "anything", nil).Allowed {
		t.Errorf("nil policy must allow")
	}
	emptyP, err := LoadPolicyFromBytes([]byte(``))
	if err != nil {
		t.Fatalf("empty policy parse: %v", err)
	}
	if !emptyP.Check("supabase", "atlas", "anything", nil).Allowed {
		t.Errorf("empty policy must allow")
	}
}

// TestPolicy_PhraseDefaultsToMatch — when the rule omits `phrase`,
// the default is the match expression. Forces a meaningful confirmation
// string instead of the user accidentally writing a rule with no
// effective phrase.
func TestPolicy_PhraseDefaultsToMatch(t *testing.T) {
	p, _ := LoadPolicyFromBytes([]byte(`
[[rule]]
match = "supabase:atlas"
confirm = ["execute_sql"]
`))
	// "yes" is not the default phrase ("supabase:atlas"), so blocked.
	d := p.Check("supabase", "atlas", "execute_sql", map[string]any{confirmKey: "yes"})
	if d.Allowed {
		t.Errorf("default phrase should not be empty/permissive")
	}
	d = p.Check("supabase", "atlas", "execute_sql", map[string]any{confirmKey: "supabase:atlas"})
	if !d.Allowed {
		t.Errorf("default phrase = match should pass when supplied")
	}
}

// TestPolicy_LoadPolicyMissingFile — a path that doesn't exist must
// return (nil, nil), not an error. Installs without policy.toml are
// the common case.
func TestPolicy_LoadPolicyMissingFile(t *testing.T) {
	p, err := LoadPolicy("/nonexistent/policy.toml")
	if err != nil {
		t.Errorf("missing file should not error, got %v", err)
	}
	if p != nil {
		t.Errorf("missing file should yield nil policy, got %+v", p)
	}
}

// TestMatchTool_Globs — small table-driven check on the glob
// matcher. Edge cases that bit me writing it: leading `*`, trailing
// `*`, double-anchor, exact match.
func TestMatchTool_Globs(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"execute_sql", "execute_sql", true},
		{"execute_sql", "execute_sqls", false},
		{"apply_*", "apply_migration", true},
		{"apply_*", "apply_", true},
		{"apply_*", "applied_things", false},
		{"*_sql", "execute_sql", true},
		{"*_sql", "sqlx_thing", false},
		{"*", "anything", true},
		{"create_*_branch", "create_dev_branch", true},
		{"create_*_branch", "create_branch", false},
		{"", "anything", false},
	}
	for _, c := range cases {
		if got := matchTool(c.pattern, c.name); got != c.want {
			t.Errorf("matchTool(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

// textOf extracts the first text-content payload from a tool result.
// Used to assert on JSON bodies the meta-tools produce.
func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil {
		t.Fatalf("nil result")
	}
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatalf("no text content in result: %+v", res)
	return ""
}
