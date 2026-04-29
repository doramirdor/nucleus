package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// helper: open a Writer in a per-test temp dir.
func newTestWriter(t *testing.T, opts Options) *Writer {
	t.Helper()
	if opts.Path == "" {
		opts.Path = filepath.Join(t.TempDir(), "audit.log")
	}
	w, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// readLines slurps the audit log; one decoded Entry per line.
func readLines(t *testing.T, path string) []Entry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("open audit: %v", err)
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("unmarshal line %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}

// TestWrite_AppendsJSONLines — the canonical happy path. Two writes
// produce two JSONL lines; each parses back into the original Entry.
func TestWrite_AppendsJSONLines(t *testing.T) {
	w := newTestWriter(t, Options{})
	for _, tool := range []string{"execute_sql", "list_tables"} {
		if err := w.Write(Entry{
			Connector: "supabase", Alias: "atlas", Tool: tool,
			Via: ViaCall, Decision: DecisionAllowed, Outcome: OutcomeOK,
			DurationMS: 12,
		}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	lines := readLines(t, w.Path())
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if lines[0].Tool != "execute_sql" || lines[1].Tool != "list_tables" {
		t.Errorf("order broken: %+v", lines)
	}
	// Auto-stamped timestamp must be set and non-zero.
	if lines[0].TS.IsZero() {
		t.Errorf("ts not stamped")
	}
}

// TestWrite_RedactsArgsByDefault — the privacy default. Args go in,
// only ArgsKeys + ArgsHash come out. Same args twice produce the
// same hash so retries can be grouped without exposing contents.
func TestWrite_RedactsArgsByDefault(t *testing.T) {
	w := newTestWriter(t, Options{})
	args := map[string]any{"query": "select * from users where id=42", "limit": 10}
	for i := 0; i < 2; i++ {
		_ = w.Write(Entry{
			Connector: "supabase", Tool: "execute_sql",
			Via: ViaCall, Decision: DecisionAllowed, Outcome: OutcomeOK,
			Args: args,
		})
	}
	lines := readLines(t, w.Path())
	if len(lines) != 2 {
		t.Fatalf("want 2 entries, got %d", len(lines))
	}
	for _, e := range lines {
		if e.Args != nil {
			t.Errorf("default writer must drop verbatim args, got %+v", e.Args)
		}
		if len(e.ArgsKeys) != 2 || e.ArgsKeys[0] != "limit" || e.ArgsKeys[1] != "query" {
			t.Errorf("ArgsKeys = %v, want sorted [limit query]", e.ArgsKeys)
		}
		if !strings.HasPrefix(e.ArgsHash, "sha256:") {
			t.Errorf("ArgsHash = %q, want sha256: prefix", e.ArgsHash)
		}
	}
	if lines[0].ArgsHash != lines[1].ArgsHash {
		t.Errorf("identical args produced different hashes: %q vs %q",
			lines[0].ArgsHash, lines[1].ArgsHash)
	}
}

// TestWrite_FullArgsOptIn — when the operator explicitly opts in,
// the verbatim args go into the log. This is the developer-debugging
// path; production should leave it off.
func TestWrite_FullArgsOptIn(t *testing.T) {
	w := newTestWriter(t, Options{FullArgs: true})
	_ = w.Write(Entry{
		Connector: "supabase", Tool: "execute_sql",
		Via: ViaCall, Decision: DecisionAllowed, Outcome: OutcomeOK,
		Args: map[string]any{"query": "select 1"},
	})
	lines := readLines(t, w.Path())
	if len(lines) != 1 {
		t.Fatalf("want 1 entry, got %d", len(lines))
	}
	if lines[0].Args["query"] != "select 1" {
		t.Errorf("opt-in did not preserve full args: %+v", lines[0].Args)
	}
}

// TestRotation_TriggersAtThreshold — push enough bytes past
// MaxFileBytes and verify rotation: audit.log resets, audit.log.1
// holds the prior content.
func TestRotation_TriggersAtThreshold(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, Options{
		Path:         filepath.Join(dir, "audit.log"),
		MaxFileBytes: 256, // tiny so a few entries trigger rotation
		MaxRotated:   3,
	})
	for i := 0; i < 10; i++ {
		_ = w.Write(Entry{
			Connector: "supabase", Alias: "atlas", Tool: "execute_sql",
			Via: ViaCall, Decision: DecisionAllowed, Outcome: OutcomeOK,
			Reason: strings.Repeat("padding-", 10),
		})
	}
	if _, err := os.Stat(filepath.Join(dir, "audit.log.1")); err != nil {
		t.Errorf("expected audit.log.1 to exist after rotation: %v", err)
	}
	// audit.log must exist (rotation reopens it immediately) but may
	// be empty if the last write was the one that triggered rotation
	// — that's fine. The contract is "the file always exists for
	// operators tailing it", not "the file always has content".
	if _, err := os.Stat(filepath.Join(dir, "audit.log")); err != nil {
		t.Errorf("active audit.log missing after rotation: %v", err)
	}
	// The rotated backup must have content — that's the audit
	// material we paid the rotation cost for.
	st1, err := os.Stat(filepath.Join(dir, "audit.log.1"))
	if err != nil {
		t.Fatalf("stat audit.log.1: %v", err)
	}
	if st1.Size() == 0 {
		t.Errorf("audit.log.1 is empty — rotation lost data")
	}
}

// TestRotation_HonorsMaxRotated — once rotated past maxRotated, the
// oldest backup is deleted instead of growing forever.
func TestRotation_HonorsMaxRotated(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, Options{
		Path:         filepath.Join(dir, "audit.log"),
		MaxFileBytes: 128,
		MaxRotated:   2,
	})
	// Many writes → many rotations.
	for i := 0; i < 50; i++ {
		_ = w.Write(Entry{
			Connector: "x", Tool: "y", Reason: strings.Repeat("z", 50),
		})
	}
	// Should have audit.log + audit.log.1 + audit.log.2 — never .3.
	if _, err := os.Stat(filepath.Join(dir, "audit.log.3")); !os.IsNotExist(err) {
		t.Errorf("audit.log.3 should not exist with maxRotated=2 (err: %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "audit.log.2")); err != nil {
		t.Errorf("audit.log.2 should exist (err: %v)", err)
	}
}

// TestConcurrentWrite_NoCorruption — 10 goroutines × 50 writes each,
// every line must parse back into a valid Entry. The mutex is the
// only thing standing between us and interleaved JSON.
func TestConcurrentWrite_NoCorruption(t *testing.T) {
	w := newTestWriter(t, Options{})
	var wg sync.WaitGroup
	const N, M = 10, 50
	for g := 0; g < N; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < M; i++ {
				_ = w.Write(Entry{
					Connector: "supabase",
					Alias:     "g" + string(rune('0'+g)),
					Tool:      "execute_sql",
					Via:       ViaCall,
					Decision:  DecisionAllowed,
					Outcome:   OutcomeOK,
				})
			}
		}()
	}
	wg.Wait()
	lines := readLines(t, w.Path())
	if len(lines) != N*M {
		t.Errorf("lost or duplicated entries: got %d, want %d", len(lines), N*M)
	}
}

// TestClose_BlocksFurtherWrites — after Close, Write returns an
// error and the file isn't appended to.
func TestClose_BlocksFurtherWrites(t *testing.T) {
	w := newTestWriter(t, Options{})
	_ = w.Write(Entry{Tool: "first"})
	_ = w.Close()
	err := w.Write(Entry{Tool: "second"})
	if err == nil {
		t.Errorf("write after close should error")
	}
	lines := readLines(t, w.Path())
	if len(lines) != 1 || lines[0].Tool != "first" {
		t.Errorf("post-close write leaked: %+v", lines)
	}
}

// TestSummarizeArgs_HashStable — same args object → same hash. This
// is the contract that makes per-call grouping work.
func TestSummarizeArgs_HashStable(t *testing.T) {
	a := map[string]any{"query": "select 1", "limit": float64(10)}
	b := map[string]any{"limit": float64(10), "query": "select 1"} // different insertion order
	_, h1 := SummarizeArgs(a)
	_, h2 := SummarizeArgs(b)
	if h1 != h2 {
		t.Errorf("hash should be insertion-order independent: %q vs %q", h1, h2)
	}
	c := map[string]any{"query": "select 2", "limit": float64(10)}
	_, h3 := SummarizeArgs(c)
	if h1 == h3 {
		t.Errorf("different args should not collide: both = %q", h1)
	}
	keys, _ := SummarizeArgs(nil)
	if keys != nil {
		t.Errorf("empty args should yield nil keys, got %v", keys)
	}
}

// TestOpen_LazyFileCreation — calling Open should NOT create the
// file. Only the first Write does. This matters for short-lived
// gateway sessions that never see a tool call.
func TestOpen_LazyFileCreation(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Options{Path: filepath.Join(dir, "audit.log")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()
	if _, err := os.Stat(w.Path()); !os.IsNotExist(err) {
		t.Errorf("audit.log should not exist before first write (err: %v)", err)
	}
	_ = w.Write(Entry{Tool: "x"})
	if _, err := os.Stat(w.Path()); err != nil {
		t.Errorf("audit.log should exist after first write: %v", err)
	}
}

// TestEntry_AutoTimestamp — when an entry is written without a TS,
// the writer stamps one. (Caller-set TS is preserved.)
func TestEntry_AutoTimestamp(t *testing.T) {
	w := newTestWriter(t, Options{})
	pre := time.Now().Add(-time.Second).UTC()
	_ = w.Write(Entry{Tool: "x"})
	lines := readLines(t, w.Path())
	if lines[0].TS.Before(pre) {
		t.Errorf("auto-stamped TS %v is before %v — clock or stamping bug", lines[0].TS, pre)
	}
	// Caller-set TS preserved.
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = w.Write(Entry{Tool: "y", TS: fixed})
	lines = readLines(t, w.Path())
	if !lines[1].TS.Equal(fixed) {
		t.Errorf("caller-set TS overwritten: got %v, want %v", lines[1].TS, fixed)
	}
}
