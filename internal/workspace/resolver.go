package workspace

import (
	"errors"
	"fmt"
	"sort"

	"github.com/doramirdor/nucleusmcp/internal/connectors"
	"github.com/doramirdor/nucleusmcp/internal/registry"
)

// Source describes why a profile was chosen.
type Source string

const (
	SourceExplicit   Source = "explicit"   // .mcp-profiles.toml named the profile
	SourceAutodetect Source = "autodetect" // manifest autodetect rule matched
	SourceOnlyOne    Source = "only_one"   // only one profile exists for the connector
	SourceDefault    Source = "default"    // user-set default via `nucleus use`
	SourceAll        Source = "all"        // fallback: expose every profile for this connector
)

// Resolution is a (connector → profile → alias) decision with provenance.
//
// When a connector has multiple bindings in .mcp-profiles.toml, the
// resolver emits one Resolution per binding. All other sources emit
// exactly one Resolution per connector.
type Resolution struct {
	Connector string
	Profile   registry.Profile
	Alias     string // used as the middle segment of proxied tool names
	Note      string // optional human note for tool descriptions (from Binding)
	Source    Source
	Hint      string
}

// Skip records a connector that had profiles registered but couldn't be
// resolved.
type Skip struct {
	Connector string
	Reason    string
}

// Resolver produces Resolutions from a workspace config + registry + cwd.
type Resolver struct {
	reg registry.Store
	cfg *Config
	cwd string
}

// NewResolver builds a resolver for the given workspace dir.
func NewResolver(reg registry.Store, cfg *Config, workspaceDir string) *Resolver {
	return &Resolver{reg: reg, cfg: cfg, cwd: workspaceDir}
}

// Resolve returns one or more Resolutions per connector plus Skips for
// connectors that couldn't be resolved.
func (r *Resolver) Resolve() ([]Resolution, []Skip, error) {
	all, err := r.reg.List()
	if err != nil {
		return nil, nil, fmt.Errorf("list profiles: %w", err)
	}

	byConnector := map[string][]registry.Profile{}
	for _, p := range all {
		byConnector[p.Connector] = append(byConnector[p.Connector], p)
	}

	// Stable output order across connectors.
	connectorNames := make([]string, 0, len(byConnector))
	for k := range byConnector {
		connectorNames = append(connectorNames, k)
	}
	sort.Strings(connectorNames)

	var resolutions []Resolution
	var skips []Skip

	for _, connector := range connectorNames {
		profiles := byConnector[connector]
		res, err := r.resolveOne(connector, profiles)
		if err != nil {
			skips = append(skips, Skip{Connector: connector, Reason: err.Error()})
			continue
		}
		resolutions = append(resolutions, res...)
	}

	return resolutions, skips, nil
}

// resolveOne runs the four-layer resolution for one connector.
// Returns one or more Resolutions — multiple only when explicit bindings
// declare several.
func (r *Resolver) resolveOne(connector string, profiles []registry.Profile) ([]Resolution, error) {
	byName := make(map[string]registry.Profile, len(profiles))
	for _, p := range profiles {
		byName[p.Name] = p
	}

	// 1. Explicit binding(s) from .mcp-profiles.toml.
	if bindings, ok := r.cfg.Bindings[connector]; ok && len(bindings) > 0 {
		out := make([]Resolution, 0, len(bindings))
		for _, b := range bindings {
			p, ok := byName[b.Profile]
			if !ok {
				return nil, fmt.Errorf(
					"binding in %s names unknown profile %q",
					r.cfg.Path, b.Profile)
			}
			out = append(out, Resolution{
				Connector: connector,
				Profile:   p,
				Alias:     b.Alias,
				Note:      b.Note,
				Source:    SourceExplicit,
				Hint:      fmt.Sprintf("from %s", r.cfg.Path),
			})
		}
		return out, nil
	}

	// 2. Autodetect from manifest rules.
	if m, ok := connectors.Get(connector); ok {
		for _, rule := range m.Autodetect {
			absPath, rel, err := FindFileInAncestors(r.cwd, rule.Files)
			if err != nil {
				return nil, fmt.Errorf("autodetect file lookup: %w", err)
			}
			if absPath == "" {
				continue
			}
			value, err := rule.Extract(absPath)
			if err != nil {
				return nil, fmt.Errorf("autodetect extract %s: %w", rel, err)
			}
			if value == "" {
				continue
			}
			for _, p := range profiles {
				if p.Metadata[rule.MatchField] == value {
					return []Resolution{{
						Connector: connector,
						Profile:   p,
						Alias:     p.Name,
						Source:    SourceAutodetect,
						Hint: fmt.Sprintf("%s=%s at %s (%s)",
							rule.MatchField, value, absPath, rule.Reason),
					}}, nil
				}
			}
			// Signal present but no profile matched — surface the fact
			// in the hint and fall through to the expose-all fallback,
			// so the user still sees tools rather than nothing.
			_ = rel
		}
	}

	// 3. Only one profile registered — implicit default.
	if len(profiles) == 1 {
		p := profiles[0]
		return []Resolution{{
			Connector: connector,
			Profile:   p,
			Alias:     p.Name,
			Source:    SourceOnlyOne,
			Hint:      "only one profile registered for this connector",
		}}, nil
	}

	// 4. User-set default.
	if def, err := r.reg.GetDefault(connector); err == nil {
		return []Resolution{{
			Connector: connector,
			Profile:   def,
			Alias:     def.Name,
			Source:    SourceDefault,
			Hint:      "user-set default via `nucleus use`",
		}}, nil
	} else if !errors.Is(err, registry.ErrNotFound) {
		return nil, fmt.Errorf("lookup default: %w", err)
	}

	// 5. Fallback: expose every profile for this connector. Claude sees
	// each one under its profile name as the alias (e.g.
	// supabase_atlas_*, supabase_default_*). This is the friendliest
	// default — users with multiple profiles get all of them visible and
	// can disambiguate by tool name, without needing to write a
	// .mcp-profiles.toml just to see anything.
	out := make([]Resolution, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, Resolution{
			Connector: connector,
			Profile:   p,
			Alias:     p.Name,
			Source:    SourceAll,
			Hint:      "no explicit binding or autodetect match — exposing all profiles",
		})
	}
	return out, nil
}
