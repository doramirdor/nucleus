package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// MetadataOption is one suggestion returned by a DiscovererFunc. The
// picker uses Label as the primary display and Summary for extra context
// (e.g. org name, region). Metadata is merged into the profile when the
// user picks this option.
type MetadataOption struct {
	Label    string
	Summary  string
	Metadata map[string]string
}

// DiscovererFunc asks the upstream MCP server for structured context
// (e.g. "list your projects") and maps the response to MetadataOptions
// that can populate a profile's metadata.
//
// Runs right after OAuth, with a live MCP client scoped to the new
// profile's credentials. Returning (nil, nil) means "nothing to suggest"
// — the caller falls back to manual prompts.
type DiscovererFunc func(ctx context.Context, c *client.Client) ([]MetadataOption, error)

// Discoverer returns the discoverer for a connector, if any.
func Discoverer(connectorName string) (DiscovererFunc, bool) {
	f, ok := discoverers[connectorName]
	return f, ok
}

// discoverers is the per-connector registry. Add a DiscovererFunc here
// to teach `nucleus add <connector>` how to auto-populate metadata
// after OAuth.
var discoverers = map[string]DiscovererFunc{
	"supabase": discoverSupabaseProjects,
}

// supabaseProject is the shape of the Supabase management API project
// record as returned by the hosted MCP's list_projects tool. We parse
// loosely — unknown fields are fine.
type supabaseProject struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Region         string `json:"region,omitempty"`
	OrganizationID string `json:"organization_id,omitempty"`
	Status         string `json:"status,omitempty"`
}

// discoverSupabaseProjects calls list_projects on the live MCP client
// and converts each project into a MetadataOption with project_id set.
//
// Defensive about response shape: Supabase's MCP has shipped at least two
// envelope styles ("[ ... ]" bare and "{ projects: [...] }"). We try
// common shapes and fall back to a generic recursive search before
// giving up.
func discoverSupabaseProjects(ctx context.Context, c *client.Client) ([]MetadataOption, error) {
	req := mcp.CallToolRequest{}
	req.Params.Name = "list_projects"
	res, err := c.CallTool(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("call list_projects: %w", err)
	}
	if res == nil || len(res.Content) == 0 {
		return nil, errors.New("list_projects returned no content")
	}

	var text strings.Builder
	for _, item := range res.Content {
		if tc, ok := item.(mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	raw := text.String()
	if raw == "" {
		return nil, errors.New("list_projects returned no text content")
	}

	projects, err := parseSupabaseProjects([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("parse list_projects response (raw=%q): %w",
			truncate(raw, 200), err)
	}

	out := make([]MetadataOption, 0, len(projects))
	for _, p := range projects {
		if p.ID == "" {
			continue
		}
		label := p.Name
		if label == "" {
			label = p.ID
		}
		summary := p.ID
		if p.Region != "" {
			summary += " · " + p.Region
		}
		if p.Status != "" && p.Status != "ACTIVE_HEALTHY" {
			summary += " · " + p.Status
		}
		out = append(out, MetadataOption{
			Label:   label,
			Summary: summary,
			Metadata: map[string]string{
				"project_id": p.ID,
			},
		})
	}
	return out, nil
}

// parseSupabaseProjects accepts a JSON payload in any of:
//
//   - bare array:    [ { "id": ..., ... }, ... ]
//   - wrapped:       { "projects": [...] }   or   { "data": [...] }   or
//                    { "result":   [...] }   or   { "items": [...] }
//   - single object: { "id": ..., ... }   (treated as a list of one)
//
// If none match, we walk the JSON tree and pick out any nested array
// whose elements have an "id" field — last-resort heuristic for
// versions of the MCP we haven't seen.
func parseSupabaseProjects(data []byte) ([]supabaseProject, error) {
	// 1) bare array
	var arr []supabaseProject
	if err := json.Unmarshal(data, &arr); err == nil {
		return arr, nil
	}

	// 2) common wrappers
	var wrappers struct {
		Projects []supabaseProject `json:"projects"`
		Data     []supabaseProject `json:"data"`
		Result   []supabaseProject `json:"result"`
		Items    []supabaseProject `json:"items"`
	}
	if err := json.Unmarshal(data, &wrappers); err == nil {
		switch {
		case len(wrappers.Projects) > 0:
			return wrappers.Projects, nil
		case len(wrappers.Data) > 0:
			return wrappers.Data, nil
		case len(wrappers.Result) > 0:
			return wrappers.Result, nil
		case len(wrappers.Items) > 0:
			return wrappers.Items, nil
		}
	}

	// 3) single object
	var single supabaseProject
	if err := json.Unmarshal(data, &single); err == nil && single.ID != "" {
		return []supabaseProject{single}, nil
	}

	// 4) generic walk — find any array of objects with an "id" field
	var any interface{}
	if err := json.Unmarshal(data, &any); err == nil {
		if found := findProjectArray(any); len(found) > 0 {
			return found, nil
		}
	}

	return nil, errors.New("no recognizable project array in response")
}

// findProjectArray walks an arbitrary JSON tree and returns the first
// array of objects-with-id it encounters.
func findProjectArray(node interface{}) []supabaseProject {
	switch v := node.(type) {
	case []interface{}:
		// Re-marshal then attempt typed unmarshal.
		if buf, err := json.Marshal(v); err == nil {
			var arr []supabaseProject
			if json.Unmarshal(buf, &arr) == nil {
				// keep only entries that actually have an ID
				out := arr[:0]
				for _, p := range arr {
					if p.ID != "" {
						out = append(out, p)
					}
				}
				if len(out) > 0 {
					return out
				}
			}
		}
		// Recurse into elements (e.g. nested object containing the list).
		for _, item := range v {
			if got := findProjectArray(item); len(got) > 0 {
				return got
			}
		}
	case map[string]interface{}:
		for _, val := range v {
			if got := findProjectArray(val); len(got) > 0 {
				return got
			}
		}
	}
	return nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
