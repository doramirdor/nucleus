package router

import (
	"strings"
	"testing"
)

func TestNamespacedName(t *testing.T) {
	got := NamespacedName("supabase", "atlas", "execute_sql")
	want := "supabase_atlas_execute_sql"
	if got != want {
		t.Errorf("NamespacedName = %q, want %q", got, want)
	}
}

func TestBuildDescriptionPrefix(t *testing.T) {
	tests := []struct {
		name      string
		connector string
		alias     string
		pc        ProfileContext
		// substrings the prefix must contain, in order
		contains []string
		// exact prefix expected, tested strictly when set
		exact string
	}{
		{
			name:      "no metadata, no note",
			connector: "supabase",
			alias:     "atlas",
			pc:        ProfileContext{},
			exact:     "[supabase/atlas]",
		},
		{
			name:      "metadata sorted deterministically",
			connector: "supabase",
			alias:     "atlas",
			pc: ProfileContext{
				Metadata: map[string]string{"zebra": "z", "apple": "a"},
			},
			// apple should come before zebra regardless of map iteration order
			contains: []string{"apple=a", "zebra=z"},
		},
		{
			name:      "with note appends ' note —'",
			connector: "supabase",
			alias:     "prod",
			pc: ProfileContext{
				Metadata: map[string]string{"project_id": "abc"},
				Note:     "PRODUCTION — read-only",
			},
			contains: []string{"[supabase/prod project_id=abc]", "PRODUCTION — read-only —"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildDescriptionPrefix(tc.connector, tc.alias, tc.pc)
			if tc.exact != "" && got != tc.exact {
				t.Errorf("prefix = %q, want %q", got, tc.exact)
			}
			for _, sub := range tc.contains {
				if !strings.Contains(got, sub) {
					t.Errorf("prefix %q does not contain %q", got, sub)
				}
			}
			// Metadata order check: "apple" must appear before "zebra".
			if tc.name == "metadata sorted deterministically" {
				if strings.Index(got, "apple") > strings.Index(got, "zebra") {
					t.Errorf("metadata not sorted: apple should come before zebra in %q", got)
				}
			}
		})
	}
}

func TestPrependDescription(t *testing.T) {
	tests := []struct {
		prefix, original, want string
	}{
		{"[x/y]", "Do a thing", "[x/y] Do a thing"},
		{"[x/y]", "", "[x/y]"},
		{"[x/y]", "   ", "[x/y]"}, // whitespace-only original treated as empty
	}
	for _, tc := range tests {
		got := prependDescription(tc.prefix, tc.original)
		if got != tc.want {
			t.Errorf("prependDescription(%q, %q) = %q, want %q",
				tc.prefix, tc.original, got, tc.want)
		}
	}
}
