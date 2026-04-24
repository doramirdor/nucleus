# multi-profile.tape — two Supabase profiles bound side-by-side via
# .mcp-profiles.toml, then the resolver's log lines that prove both
# aliases wire up in one gateway session.
#
# Regenerate with:  vhs demo/multi-profile.tape

Output demo/multi-profile.gif

Set Shell "bash"
Set FontSize 14
Set Width 1200
Set Height 900
Set Theme "Catppuccin Mocha"
Set TypingSpeed 45ms

# Setup: scratch dir, clean prompt. All hidden.
Hide
Type `PS1='$ ' && cd $(mktemp -d -t nucleus-multi.XXX) && clear`
Enter
Sleep 400ms
Show

# Show the two registered profiles
Type "nucleus list"
Enter
Sleep 3s

Type "# Expose both under short aliases via .mcp-profiles.toml"
Enter
Sleep 1s

# Write the config in one hidden printf so the heredoc continuation
# prompts don't clutter the GIF. The file is shown via `cat` right after.
Hide
Type `printf '[[supabase]]\nprofile = "atlas"\nalias   = "prod"\nnote    = "PRODUCTION — read-only unless explicitly asked"\n\n[[supabase]]\nprofile = "default"\nalias   = "staging"\nnote    = "staging"\n' > .mcp-profiles.toml`
Enter
Sleep 300ms
Show

Type "cat .mcp-profiles.toml"
Enter
Sleep 4s

Type "# starting the gateway logs which profile each alias binds to:"
Enter
Sleep 800ms
Type "echo '' | nucleus serve 2>&1 >/dev/null | grep -E 'workspace|resolved profile|alias ready' | head -8"
Enter
Sleep 6s

Sleep 1s
