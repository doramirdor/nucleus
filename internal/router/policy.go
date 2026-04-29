// Package router — policy gate.
//
// The policy gate is the difference between "Nucleus is a clever dev
// toy" and "I'd hand this to a junior on the team." It runs inside
// makeHandler before the call reaches the upstream, so every dispatch
// path — direct tools (ModeExposeAll), nucleus_call, nucleus_call_plan
// steps — is gated identically.
//
// Two enforcement modes per rule:
//
//   - deny: the tool is blocked outright. The error message names the
//     rule so the user can find and edit it.
//   - confirm: the tool is allowed only when the caller's arguments
//     include the magic confirmation phrase (under the
//     `__nucleus_confirm` key). On the first call without the phrase,
//     the gate returns a structured error telling the LLM the exact
//     string to include — so the second call after a one-line nudge
//     succeeds. The phrase ends up in audit logs alongside the call,
//     which is the whole point: a deliberate, attributable confirmation.
//
// Rules match on `<connector>:<alias>` with `*` wildcards on either
// side. Tool name patterns are simple globs (`*` matches any run of
// characters). When multiple rules match, deny wins over confirm.
package router

import (
	"errors"
	"fmt"
	"os"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// confirmKey is the single argument key the gate looks at to detect a
// confirmation phrase. Underscored prefix so it can't collide with any
// real upstream argument name (MCP tools never use double-underscore
// args by convention).
const confirmKey = "__nucleus_confirm"

// Policy is the loaded set of rules consulted on every dispatch.
// Construct via LoadPolicy or LoadPolicyFromBytes; the zero value is
// useless.
type Policy struct {
	Rules []Rule
}

// Rule is one row in policy.toml. Match selects which profiles it
// applies to; Deny and Confirm are mutually exclusive *patterns* (a
// rule can list both, but a tool matching Deny short-circuits Confirm
// on the same rule).
type Rule struct {
	// Match is "<connector>:<alias>", with `*` wildcards. Examples:
	//   "supabase:atlas"  → exact profile
	//   "supabase:*"      → all supabase profiles
	//   "*:*"             → every profile (think very carefully)
	Match string `toml:"match"`

	// Deny lists tool-name globs that are blocked outright. Any match
	// → call returns an error before reaching the upstream.
	Deny []string `toml:"deny"`

	// Confirm lists tool-name globs that require the caller to send
	// `__nucleus_confirm: "<phrase>"` in the arguments. Without the
	// phrase the call is blocked; with it the call passes through.
	Confirm []string `toml:"confirm"`

	// Phrase is the exact confirmation string this rule expects. If
	// empty, the rule's Match value is used so the phrase is never
	// silently absent.
	Phrase string `toml:"phrase"`

	// Reason is an optional human-readable note surfaced in the
	// blocking error message ("PROD — protect the prod DB", etc.).
	// Helps the operator who reads the error figure out why the rule
	// exists and whether to override it.
	Reason string `toml:"reason"`
}

// Decision is the verdict from Policy.Check. Allowed=true means
// "forward the call"; Allowed=false means "return Message as a tool
// error and don't dispatch."
type Decision struct {
	Allowed bool
	Message string
}

// LoadPolicy reads a policy.toml from disk. A missing file is *not*
// an error — installs without a policy.toml are the common case and
// "no policy = allow everything." Returns nil, nil for missing files.
func LoadPolicy(path string) (*Policy, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read policy %s: %w", path, err)
	}
	return LoadPolicyFromBytes(b)
}

// LoadPolicyFromBytes parses policy TOML directly — split out for
// tests so they don't have to write to disk.
func LoadPolicyFromBytes(data []byte) (*Policy, error) {
	var doc struct {
		Rule []Rule `toml:"rule"`
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse policy toml: %w", err)
	}
	// Empty file → empty policy → "allow everything." Keep that as
	// the explicit no-op semantics rather than returning nil and
	// branching at the call site.
	p := &Policy{Rules: doc.Rule}
	for i := range p.Rules {
		if p.Rules[i].Phrase == "" {
			// Default phrase: the match expression itself. Forces the
			// caller to type a meaningful string ("supabase:atlas")
			// rather than something like "yes" that anyone could
			// blindly include in every call.
			p.Rules[i].Phrase = p.Rules[i].Match
		}
	}
	return p, nil
}

// Check evaluates the policy against a single dispatch. Returns Allowed
// when no matching rule blocks; otherwise returns a Decision whose
// Message is ready to be returned to the caller as a tool-error.
//
// Evaluation order: every matching rule is considered. A Deny match
// short-circuits with the first hit. Otherwise, all Confirm matches
// are collected and any one with a missing phrase blocks the call.
func (p *Policy) Check(connector, alias, tool string, args map[string]any) Decision {
	if p == nil || len(p.Rules) == 0 {
		return Decision{Allowed: true}
	}
	provided, hasPhrase := extractConfirmPhrase(args)

	for _, r := range p.Rules {
		if !matchProfile(r.Match, connector, alias) {
			continue
		}
		for _, pat := range r.Deny {
			if matchTool(pat, tool) {
				return Decision{
					Allowed: false,
					Message: formatDeny(r, connector, alias, tool, pat),
				}
			}
		}
	}
	for _, r := range p.Rules {
		if !matchProfile(r.Match, connector, alias) {
			continue
		}
		for _, pat := range r.Confirm {
			if !matchTool(pat, tool) {
				continue
			}
			if !hasPhrase || provided != r.Phrase {
				return Decision{
					Allowed: false,
					Message: formatConfirm(r, connector, alias, tool, provided),
				}
			}
		}
	}
	return Decision{Allowed: true}
}

func extractConfirmPhrase(args map[string]any) (string, bool) {
	v, ok := args[confirmKey]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// matchProfile evaluates a "<connector>:<alias>" pattern. Either side
// can be `*` for "any". Case-insensitive. Malformed patterns (no
// colon) are rejected — silently skipping them would hide a typo'd
// rule that the user expected to enforce something.
func matchProfile(pattern, connector, alias string) bool {
	pattern = strings.TrimSpace(strings.ToLower(pattern))
	if pattern == "" {
		return false
	}
	parts := strings.SplitN(pattern, ":", 2)
	if len(parts) != 2 {
		return false
	}
	c, a := parts[0], parts[1]
	if c != "*" && c != strings.ToLower(connector) {
		return false
	}
	if a != "*" && a != strings.ToLower(alias) {
		return false
	}
	return true
}

// matchTool evaluates a glob with `*` as the only wildcard. We
// intentionally don't pull in path/filepath.Match — its escape rules
// (`[`, `]`, `?`) are surprising in a YAML/TOML editing context and
// nobody writing a policy expects character classes.
func matchTool(pattern, name string) bool {
	pattern = strings.ToLower(pattern)
	name = strings.ToLower(name)
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == name
	}
	parts := strings.Split(pattern, "*")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		idx := strings.Index(name[pos:], part)
		if idx < 0 {
			return false
		}
		// First non-empty fragment must anchor at the start unless
		// the pattern starts with `*`.
		if i == 0 && !strings.HasPrefix(pattern, "*") && idx != 0 {
			return false
		}
		pos += idx + len(part)
	}
	// Last non-empty fragment must reach the end unless the pattern
	// ends with `*`.
	if !strings.HasSuffix(pattern, "*") {
		// Find the last non-empty fragment.
		var last string
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i] != "" {
				last = parts[i]
				break
			}
		}
		if last != "" && !strings.HasSuffix(name, last) {
			return false
		}
	}
	return true
}

func formatDeny(r Rule, connector, alias, tool, pat string) string {
	reason := r.Reason
	if reason == "" {
		reason = fmt.Sprintf("policy denies %s on %s:%s", pat, connector, alias)
	}
	return fmt.Sprintf(
		"BLOCKED by policy: tool %q on profile %s:%s is denied (rule match=%q, pattern=%q). Reason: %s",
		tool, connector, alias, r.Match, pat, reason)
}

func formatConfirm(r Rule, connector, alias, tool, provided string) string {
	reason := r.Reason
	if reason != "" {
		reason = " " + reason
	}
	if provided == "" {
		return fmt.Sprintf(
			"CONFIRMATION REQUIRED: tool %q on profile %s:%s requires explicit confirmation.%s "+
				"Re-call this tool with an extra argument %q set to %q to proceed.",
			tool, connector, alias, reason, confirmKey, r.Phrase)
	}
	return fmt.Sprintf(
		"CONFIRMATION MISMATCH: tool %q on profile %s:%s requires %q=%q but got %q. "+
			"Re-call with the exact phrase to proceed.",
		tool, connector, alias, confirmKey, r.Phrase, provided)
}
