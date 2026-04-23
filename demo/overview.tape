# overview.tape — hero GIF for README
#
# Regenerate with:
#   vhs demo/overview.tape
#
# Requires:
#   - vhs installed      (https://github.com/charmbracelet/vhs)
#   - nucleusmcp on PATH (`make install`)
#   - at least one profile registered (e.g. via `nucleusmcp add supabase`)

Output demo/overview.gif

Set Shell "bash"
Set FontSize 14
Set Width 1200
Set Height 720
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms
Set PlaybackSpeed 1.0

# Setup: drop into a clean scratch dir so this demo doesn't pick up a
# .mcp-profiles.toml from the repo we're recording from. Hidden from the GIF.
Hide
Type `SCRATCH=$(mktemp -d -t nucleus-overview.XXX) && cd "$SCRATCH" && clear`
Enter
Sleep 500ms
Show

# 1. Built-in + custom connectors
Type "nucleusmcp connectors"
Sleep 400ms
Enter
Sleep 3s

# 2. Active profiles (connections)
Type "nucleusmcp list"
Sleep 400ms
Enter
Sleep 3s

# 3. Config for one profile (no live probe — keeps GIF deterministic)
Type "nucleusmcp info --no-probe"
Sleep 400ms
Enter
Sleep 5s

# 4. Register with Claude Code (print the snippet without writing)
Type "nucleusmcp install --print"
Sleep 400ms
Enter
Sleep 5s

Sleep 1s

Hide
Type `cd ~ && rm -rf "$SCRATCH"`
Enter
Show
