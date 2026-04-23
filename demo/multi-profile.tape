# multi-profile.tape — showcases two Supabase profiles side-by-side,
# the ".mcp-profiles.toml" binding, and the namespaced tool names
# produced by the router.
#
# Regenerate with:  vhs demo/multi-profile.tape
#
# Requires two `supabase:*` profiles already added.

Output demo/multi-profile.gif

Set Shell "bash"
Set FontSize 14
Set Width 1200
Set Height 820
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms

# Setup: scratch dir, hidden from the GIF so we start with a clean prompt.
Hide
Type `SCRATCH=$(mktemp -d -t nucleus-multi.XXX) && cd "$SCRATCH" && clear`
Enter
Sleep 500ms
Show

Type "# Two Supabase profiles — both live in the same Claude session"
Sleep 1.5s
Enter
Sleep 400ms

Type "nucleusmcp list"
Enter
Sleep 3s

# Write a .mcp-profiles.toml that exposes both with friendly aliases + notes.
Type "cat > .mcp-profiles.toml <<'EOF'"
Enter
Type "[[supabase]]"
Enter
Type 'profile = "atlas"'
Enter
Type 'alias   = "prod"'
Enter
Type 'note    = "PRODUCTION — read-only unless explicitly asked"'
Enter
Type ""
Enter
Type "[[supabase]]"
Enter
Type 'profile = "default"'
Enter
Type 'alias   = "staging"'
Enter
Type 'note    = "staging"'
Enter
Type "EOF"
Enter
Sleep 1s

Type "cat .mcp-profiles.toml"
Enter
Sleep 3s

# The resolver's view, printed as log lines on gateway startup.
Type "# starting the gateway logs which profile each alias binds to:"
Enter
Sleep 500ms
Type "echo '' | nucleusmcp serve 2>&1 >/dev/null | grep -E 'workspace|resolved profile|alias ready|gateway listening' | head -8"
Enter
Sleep 5s

Sleep 1.5s

Hide
Type `cd ~ && rm -rf "$SCRATCH"`
Enter
Show
