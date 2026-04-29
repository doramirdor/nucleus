// `nucleus logs` reads the JSONL audit trail the gateway writes for
// every dispatch. The default behavior is "tail of the last 50
// entries pretty-printed"; flags filter by connector / alias / tool /
// outcome / age, and --json passes the raw JSONL through for piping
// into jq or downstream tooling.
//
// Why a CLI command rather than just `tail -f ~/.nucleusmcp/audit.log`?
// Two reasons:
//   - Rotation. The active log can roll mid-tail; this command stitches
//     audit.log + audit.log.1..N together in time order so older
//     filtering ("--since 24h") doesn't silently drop pre-rotation
//     entries.
//   - Privacy. Pretty-print elides ArgsHash and the long IDs by
//     default so the terminal output is readable; structured callers
//     opt back in with --json.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/doramirdor/nucleusmcp/internal/audit"
)

func newLogsCmd() *cobra.Command {
	var (
		path        string
		jsonOut     bool
		last        int
		connector   string
		alias       string
		tool        string
		outcome     string
		decision    string
		since       time.Duration
		showHash    bool
		showArgs    bool
	)
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Tail the audit log",
		Long: "Tail the gateway's per-call audit trail. Filters compose " +
			"(intersection): --connector supabase --alias atlas --tool execute_sql " +
			"shows only execute_sql calls on supabase:atlas. --json prints the raw " +
			"JSONL for piping into jq.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if path == "" {
				p, err := audit.DefaultPath()
				if err != nil {
					return fmt.Errorf("resolve default path: %w", err)
				}
				path = p
			}
			entries, err := readAllRotated(path)
			if err != nil {
				return err
			}
			entries = filterEntries(entries, filterSpec{
				connector: connector, alias: alias, tool: tool,
				outcome:   outcome, decision: decision,
				since:     since,
			})
			// Tail-N happens after filtering — `--last 50 --tool foo`
			// means "the 50 most recent foo calls", which is what
			// users expect.
			if last > 0 && len(entries) > last {
				entries = entries[len(entries)-last:]
			}
			if jsonOut {
				return printJSONL(entries)
			}
			return printPretty(entries, showHash, showArgs)
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "audit log path (default: ~/.nucleusmcp/audit.log)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit raw JSONL instead of pretty-printing")
	cmd.Flags().IntVarP(&last, "last", "n", 50, "show only the most recent N entries (0 = all)")
	cmd.Flags().StringVar(&connector, "connector", "", "filter by connector (e.g. supabase)")
	cmd.Flags().StringVar(&alias, "alias", "", "filter by alias (e.g. atlas)")
	cmd.Flags().StringVar(&tool, "tool", "", "filter by upstream tool name (e.g. execute_sql)")
	cmd.Flags().StringVar(&outcome, "outcome", "", "filter by outcome (ok, upstream-error, transport-error, blocked)")
	cmd.Flags().StringVar(&decision, "decision", "", "filter by decision (allowed, denied, confirm-required, confirm-mismatch)")
	cmd.Flags().DurationVar(&since, "since", 0, "only show entries newer than this duration (e.g. 24h, 30m)")
	cmd.Flags().BoolVar(&showHash, "show-hash", false, "include args hash in pretty output")
	cmd.Flags().BoolVar(&showArgs, "show-args", false, "include full args (only present if NUCLEUSMCP_AUDIT_FULL_ARGS=1 was set on the writer)")
	return cmd
}

// readAllRotated reads audit.log, audit.log.1, audit.log.2, ... and
// merges them in chronological order. Rotated files are older than
// the active one; within rotated, .1 is newer than .2 etc. Within a
// single file, lines are append-only so reading in order is correct.
func readAllRotated(active string) ([]audit.Entry, error) {
	dir := filepath.Dir(active)
	base := filepath.Base(active)

	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// No audit dir yet → nothing to show. Not an error; the
			// gateway just hasn't been used yet.
			return nil, nil
		}
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}

	// Collect rotated files sorted by descending suffix number
	// (.5 → oldest, .1 → most recent backup), then the active log
	// last so the merged stream ends with the newest entries.
	type rot struct {
		path string
		n    int
	}
	var rotated []rot
	for _, de := range dirEntries {
		if de.IsDir() || !strings.HasPrefix(de.Name(), base+".") {
			continue
		}
		suffix := strings.TrimPrefix(de.Name(), base+".")
		n := 0
		for _, c := range suffix {
			if c < '0' || c > '9' {
				n = -1
				break
			}
			n = n*10 + int(c-'0')
		}
		if n <= 0 {
			continue
		}
		rotated = append(rotated, rot{path: filepath.Join(dir, de.Name()), n: n})
	}
	sort.Slice(rotated, func(i, j int) bool { return rotated[i].n > rotated[j].n })

	var all []audit.Entry
	for _, r := range rotated {
		es, err := readJSONL(r.path)
		if err != nil {
			return nil, err
		}
		all = append(all, es...)
	}
	if _, err := os.Stat(active); err == nil {
		es, err := readJSONL(active)
		if err != nil {
			return nil, err
		}
		all = append(all, es...)
	}
	return all, nil
}

func readJSONL(path string) ([]audit.Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	var out []audit.Entry
	sc := bufio.NewScanner(f)
	// Some entries can carry long Reason strings (upstream error
	// dumps). Bump the per-line cap from the 64KiB default so we
	// don't silently truncate.
	sc.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e audit.Entry
		if err := json.Unmarshal(line, &e); err != nil {
			// One malformed line shouldn't kill the whole tail —
			// surface it once and keep going. The audit log is
			// observability, not a transactional store.
			fmt.Fprintf(os.Stderr, "logs: skipping malformed line in %s: %v\n", path, err)
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

type filterSpec struct {
	connector, alias, tool string
	outcome, decision      string
	since                  time.Duration
}

func filterEntries(in []audit.Entry, f filterSpec) []audit.Entry {
	if f.connector == "" && f.alias == "" && f.tool == "" &&
		f.outcome == "" && f.decision == "" && f.since == 0 {
		return in
	}
	cutoff := time.Time{}
	if f.since > 0 {
		cutoff = time.Now().Add(-f.since)
	}
	out := in[:0:0]
	for _, e := range in {
		if f.connector != "" && !strings.EqualFold(e.Connector, f.connector) {
			continue
		}
		if f.alias != "" && !strings.EqualFold(e.Alias, f.alias) {
			continue
		}
		if f.tool != "" && !strings.EqualFold(e.Tool, f.tool) {
			continue
		}
		if f.outcome != "" && !strings.EqualFold(string(e.Outcome), f.outcome) {
			continue
		}
		if f.decision != "" && !strings.EqualFold(string(e.Decision), f.decision) {
			continue
		}
		if !cutoff.IsZero() && e.TS.Before(cutoff) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func printJSONL(entries []audit.Entry) error {
	enc := json.NewEncoder(os.Stdout)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}

// printPretty emits a tabular view sized for a 120-column terminal.
// Column choices come from "what does an operator skim for?": time,
// outcome (the eye-grabber for failures), profile, tool, duration,
// and a one-liner reason that gets truncated to keep rows on one
// line. Optional columns add the args hash and verbatim args.
func printPretty(entries []audit.Entry, showHash, showArgs bool) error {
	if len(entries) == 0 {
		fmt.Println("(no audit entries)")
		return nil
	}
	for _, e := range entries {
		ts := e.TS.Local().Format("2006-01-02 15:04:05")
		profile := e.Connector + ":" + e.Alias
		marker := outcomeMarker(e)
		dur := fmt.Sprintf("%4dms", e.DurationMS)
		via := string(e.Via)
		if via == "" {
			via = "-"
		}
		// Reason is the most variable column; cap it so a 2KB
		// upstream stack trace doesn't blow up the layout.
		reason := strings.ReplaceAll(e.Reason, "\n", " ⏎ ")
		if len(reason) > 80 {
			reason = reason[:77] + "..."
		}
		fmt.Printf("%s  %s  %-22s  %-22s  %s  %-18s  %s\n",
			ts, marker, profile, e.Tool, dur, via, reason)
		if showHash && e.ArgsHash != "" {
			fmt.Printf("%24s    args_hash=%s keys=%v\n",
				"", e.ArgsHash[:23]+"...", e.ArgsKeys)
		}
		if showArgs && e.Args != nil {
			b, _ := json.Marshal(e.Args)
			fmt.Printf("%24s    args=%s\n", "", string(b))
		}
	}
	return nil
}

// outcomeMarker is a single glyph that lets the eye scan a long log
// for failures: "OK", "ERR", "BLK", or the raw outcome if we don't
// recognize it. Short, fixed width, no ANSI — keeps the column
// alignment clean even when stdout isn't a TTY.
func outcomeMarker(e audit.Entry) string {
	switch e.Outcome {
	case audit.OutcomeOK:
		return "OK "
	case audit.OutcomeBlocked:
		return "BLK"
	case audit.OutcomeUpstreamError, audit.OutcomeTransportErr:
		return "ERR"
	default:
		if string(e.Outcome) == "" {
			return "?? "
		}
		return string(e.Outcome[0:3])
	}
}
