// Package audit writes a per-call audit trail for every dispatch the
// gateway sees — direct tool calls, nucleus_call invocations, and each
// step of nucleus_call_plan. It exists so an operator (or a future
// `nucleus logs` consumer) can answer two questions cheaply:
//
//  1. *What did Claude actually do on which profile?* — needed for
//     incident review, compliance, and "did this destructive op
//     actually run on prod, or did the policy gate catch it?"
//  2. *Why did the gateway pick this profile/decision?* — captured
//     via the policy decision and (when applicable) the sticky/recent
//     state.
//
// # Privacy posture
//
// Tool arguments often carry user data — SQL queries, repo paths,
// possibly even PII. Logging them verbatim is a footgun. The default
// is therefore to log only argument *keys* and a SHA-256 hash of the
// whole argument object (so two identical calls group together
// without exposing contents). Set NUCLEUSMCP_AUDIT_FULL_ARGS=1 to opt
// into full-argument logging — useful for local debugging, but the
// onus is on the operator to know what their tools traffic in.
//
// Result payloads are NEVER logged — they're often the largest part
// of an MCP exchange and the most likely to contain customer data.
// The audit records success/failure and duration; the actual result
// stays between the gateway and the client.
//
// # Format
//
// One JSON object per line (JSONL). Files are append-only;
// concurrent writes from multiple goroutines are serialized through
// a single mutex. Rotation is size-based: when the active file
// exceeds maxFileBytes, it's renamed to audit.log.1 (older entries
// shifting to .2, .3, …) and a fresh audit.log starts. We keep up
// to maxRotated rotated files; older ones are deleted on rotation.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Standard rotation thresholds. Picked so an active gateway with a
// chatty MCP client (~100 calls/day) keeps weeks of history without
// requiring the operator to think about disk usage. Override at the
// call site if you need different values.
const (
	defaultMaxFileBytes = 10 * 1024 * 1024 // 10 MiB
	defaultMaxRotated   = 5                // total cap ≈ 60 MiB
)

// Decision captures the policy gate's verdict for an entry. The
// stringly-typed enum keeps the JSONL human-greppable
// (`grep '"decision":"denied"' audit.log`) without needing a Go
// const-stringer dance.
type Decision string

const (
	DecisionAllowed         Decision = "allowed"
	DecisionDenied          Decision = "denied"
	DecisionConfirmRequired Decision = "confirm-required"
	DecisionConfirmMismatch Decision = "confirm-mismatch"
)

// Outcome records what the upstream actually did with the call after
// the gate passed. Distinct from Decision because a policy-allowed
// call can still fail upstream (network, auth, schema mismatch).
type Outcome string

const (
	OutcomeOK            Outcome = "ok"
	OutcomeUpstreamError Outcome = "upstream-error"
	OutcomeTransportErr  Outcome = "transport-error"
	OutcomeBlocked       Outcome = "blocked"  // policy denied — no upstream call
)

// Via identifies which dispatch path produced this entry. Useful for
// "are people actually using nucleus_call_plan?" usage analysis and
// for distinguishing the meta-tool-driven calls from legacy
// expose-all direct calls.
type Via string

const (
	ViaDirect   Via = "direct"
	ViaCall     Via = "nucleus_call"
	ViaCallPlan Via = "nucleus_call_plan"
)

// Entry is one audit row. Field names stay short so the JSONL is
// scannable in a terminal without horizontal scrolling.
type Entry struct {
	TS         time.Time `json:"ts"`
	Connector  string    `json:"connector"`
	Alias      string    `json:"alias"`
	ProfileID  string    `json:"profile_id,omitempty"`
	Tool       string    `json:"tool"`
	Via        Via       `json:"via"`
	Decision   Decision  `json:"decision"`
	Outcome    Outcome   `json:"outcome"`
	DurationMS int64     `json:"duration_ms"`

	// ArgsKeys lists the top-level argument keys present on the
	// call. Cheap to log, useful for grouping ("which of these calls
	// included a query argument?"), and PII-safe.
	ArgsKeys []string `json:"args_keys,omitempty"`
	// ArgsHash is a SHA-256 of the canonical-JSON-encoded arguments.
	// Two byte-identical argument objects produce the same hash, so
	// you can spot retries / loops without seeing the contents.
	ArgsHash string `json:"args_hash,omitempty"`
	// Args is the raw argument object — only populated when
	// NUCLEUSMCP_AUDIT_FULL_ARGS=1. Default is empty.
	Args map[string]any `json:"args,omitempty"`

	// Reason is a free-form explanation. For denials this is the
	// policy's blocking message; for upstream errors it's the error
	// string. Always present when Outcome != OK.
	Reason string `json:"reason,omitempty"`

	// Meta is a small bag of optional extras the caller can set —
	// e.g. "plan_index": 2 for nucleus_call_plan steps, "from_sticky":
	// true when the recommender used sticky bias. Keep it tight: this
	// is structured logging, not a kitchen sink.
	Meta map[string]any `json:"meta,omitempty"`
}

// Writer is the audit sink. Safe for concurrent use; a single mutex
// serializes all writes so the file ends up as well-formed JSONL.
// Construct via Open — the zero value is unusable.
type Writer struct {
	mu sync.Mutex

	path         string
	fullArgs     bool
	maxFileBytes int64
	maxRotated   int

	// f is the active file handle. It can be nil when audit was
	// configured but the file hasn't been opened yet (lazy on first
	// write to avoid creating empty audit.log files for installs that
	// never call a tool in a session).
	f *os.File
	// size tracks the current file's size so we don't stat() every
	// write. Initialized when f opens.
	size int64
	// closed prevents writes after Close() — racy clients trying to
	// log during shutdown get a no-op rather than a panic.
	closed bool
}

// Options configure a Writer. Zero values mean "use defaults"; this
// is the path most callers should take.
type Options struct {
	// Path is the active log file. Defaults to ~/.nucleusmcp/audit.log
	// when empty (resolved by DefaultPath).
	Path string
	// FullArgs disables PII protection and logs full argument
	// objects. Defaults to false; the env var
	// NUCLEUSMCP_AUDIT_FULL_ARGS=1 sets it via Open.
	FullArgs bool
	// MaxFileBytes overrides the rotation threshold.
	MaxFileBytes int64
	// MaxRotated overrides the retained-rotation count.
	MaxRotated int
}

// DefaultPath returns ~/.nucleusmcp/audit.log. Symmetric with the
// other ~/.nucleusmcp/* paths used by registry, vault, etc.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".nucleusmcp", "audit.log"), nil
}

// Open builds a Writer. The file isn't opened until the first Write,
// so a gateway that never sees a tool call won't create a log file.
//
// The env var NUCLEUSMCP_AUDIT_FULL_ARGS=1 turns on full-argument
// logging — convenient for local debugging without recompiling.
func Open(opts Options) (*Writer, error) {
	w := &Writer{
		path:         opts.Path,
		fullArgs:     opts.FullArgs,
		maxFileBytes: opts.MaxFileBytes,
		maxRotated:   opts.MaxRotated,
	}
	if w.path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, fmt.Errorf("default audit path: %w", err)
		}
		w.path = p
	}
	if w.maxFileBytes <= 0 {
		w.maxFileBytes = defaultMaxFileBytes
	}
	if w.maxRotated <= 0 {
		w.maxRotated = defaultMaxRotated
	}
	if os.Getenv("NUCLEUSMCP_AUDIT_FULL_ARGS") == "1" {
		w.fullArgs = true
	}
	return w, nil
}

// Path returns the resolved audit file path. Useful for tests and
// for `nucleus logs` to find what to read.
func (w *Writer) Path() string { return w.path }

// Write appends one entry. Errors are returned but the caller is
// expected to log-and-drop them rather than fail the dispatch — an
// audit failure must never break user-visible behavior.
func (w *Writer) Write(e Entry) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("audit writer closed")
	}

	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	// Compute the privacy-safe summary BEFORE we decide whether to
	// drop the verbatim args. Callers can pre-populate ArgsKeys /
	// ArgsHash themselves (e.g. router.writeAudit does) but if they
	// haven't, doing it here means a single audit.Writer.Write call
	// is enough — no contract that callers must remember to summarize.
	if e.Args != nil && (len(e.ArgsKeys) == 0 || e.ArgsHash == "") {
		keys, hash := SummarizeArgs(e.Args)
		if len(e.ArgsKeys) == 0 {
			e.ArgsKeys = keys
		}
		if e.ArgsHash == "" {
			e.ArgsHash = hash
		}
	}
	if !w.fullArgs {
		// Drop the verbatim args; keep only the privacy-safe summary.
		e.Args = nil
	}

	if err := w.ensureOpenLocked(); err != nil {
		return err
	}

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("audit marshal: %w", err)
	}
	line = append(line, '\n')

	n, err := w.f.Write(line)
	if err != nil {
		return fmt.Errorf("audit write: %w", err)
	}
	w.size += int64(n)

	if w.size >= w.maxFileBytes {
		// Rotation failure must not kill the writer — log the issue
		// and keep going on the existing file. Worst case the file
		// grows past the threshold; that's strictly better than
		// dropping audit entries.
		if rerr := w.rotateLocked(); rerr != nil {
			return fmt.Errorf("audit rotate: %w (entry was written)", rerr)
		}
	}
	return nil
}

// Close releases the file handle. Subsequent writes return an error.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.f != nil {
		err := w.f.Close()
		w.f = nil
		return err
	}
	return nil
}

func (w *Writer) ensureOpenLocked() error {
	if w.f != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
		return fmt.Errorf("audit mkdir: %w", err)
	}
	// 0o600: audit log entries can hint at internal structure even
	// when args are redacted; tighten by default so a multi-user
	// machine doesn't expose them.
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("audit open: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("audit stat: %w", err)
	}
	w.f = f
	w.size = st.Size()
	return nil
}

// rotateLocked shifts audit.log → audit.log.1 → audit.log.2 → … up
// to maxRotated, deleting the file that would have spilled past the
// cap. After rotation, an empty audit.log is reopened immediately
// so operators tailing the file always have something to point at.
// Caller holds w.mu.
//
// Order matters here. Delete the oldest first, THEN shift each
// remaining backup up by one slot, finally rename the active log to
// .1. Doing it backwards (or skipping the explicit oldest-delete
// step) is what kept causing tests to find no audit.log.2 after
// repeated rotations — see the test history for the bug.
func (w *Writer) rotateLocked() error {
	if w.f == nil {
		return nil
	}
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("close before rotate: %w", err)
	}
	w.f = nil
	w.size = 0

	// 1. Drop whatever's in the highest-numbered slot — it's about
	//    to be overwritten by the slot one below it.
	oldest := w.path + "." + itoa(w.maxRotated)
	if _, err := os.Stat(oldest); err == nil {
		if err := os.Remove(oldest); err != nil {
			return fmt.Errorf("remove oldest %s: %w", oldest, err)
		}
	}
	// 2. Shift .{N-1} → .N, ..., .1 → .2.
	for i := w.maxRotated - 1; i >= 1; i-- {
		src := w.path + "." + itoa(i)
		dst := w.path + "." + itoa(i+1)
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("rename %s → %s: %w", src, dst, err)
		}
	}
	// 3. Move the active log into the .1 slot.
	if _, err := os.Stat(w.path); err == nil {
		if err := os.Rename(w.path, w.path+".1"); err != nil {
			return fmt.Errorf("rename active → .1: %w", err)
		}
	}
	// 4. Reopen the active log immediately so a tail or
	//    `nucleus logs` finds the file even if no further write
	//    happens before the operator looks.
	return w.ensureOpenLocked()
}

func itoa(i int) string {
	// Avoiding strconv to keep the audit file tiny and the import
	// list short — these numbers are 1..maxRotated, single digits in
	// every realistic configuration.
	if i < 10 {
		return string(rune('0' + i))
	}
	return fmt.Sprintf("%d", i)
}

// SummarizeArgs builds the privacy-safe args summary: sorted top-
// level keys + a SHA-256 hash of canonical JSON. Exposed so tests
// can assert on the same shape the production writer produces.
func SummarizeArgs(args map[string]any) (keys []string, hash string) {
	if len(args) == 0 {
		return nil, ""
	}
	keys = make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Canonical JSON: marshal a map[string]any with sorted keys via
	// a per-call shim. encoding/json sorts map keys deterministically
	// since Go 1.12, so a direct Marshal is canonical for the keys
	// present (nested maps have the same property). Good enough for
	// the "two identical args produce the same hash" guarantee.
	b, err := json.Marshal(args)
	if err != nil {
		// Fall back to a hash of the keys list — better than nothing.
		b = []byte(fmt.Sprintf("%v", keys))
	}
	sum := sha256.Sum256(b)
	hash = "sha256:" + hex.EncodeToString(sum[:])
	return keys, hash
}
