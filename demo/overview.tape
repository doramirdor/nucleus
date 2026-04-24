# overview.tape — hero GIF for README
#
# Regenerate with:  vhs demo/overview.tape
#
# Requires:
#   - vhs installed        (https://github.com/charmbracelet/vhs)
#   - nucleus on PATH      (`make install`)
#   - at least one profile registered

Output demo/overview.gif

Set Shell "bash"
Set FontSize 14
Set Width 1200
Set Height 900
Set Theme "Catppuccin Mocha"
Set TypingSpeed 45ms
Set PlaybackSpeed 1.0

# Setup: scratch dir + clean prompt. Hidden so the GIF starts clean.
Hide
Type `PS1='$ ' && cd $(mktemp -d -t nucleus-overview.XXX) && clear`
Enter
Sleep 400ms
Show

# 1. Built-in + custom connectors
Type "nucleus connectors"
Sleep 300ms
Enter
Sleep 3s

# 2. Active profiles
Type "nucleus list"
Sleep 300ms
Enter
Sleep 3s

# 3. Per-profile config (no live probe so the GIF is deterministic)
Type "nucleus info --no-probe"
Sleep 300ms
Enter
Sleep 6s

# 4. Register with Claude (print-only)
Type "nucleus install --print"
Sleep 300ms
Enter
Sleep 5s

Sleep 800ms
