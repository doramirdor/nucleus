# Demo GIFs

The GIFs in the main README are generated from scripted [vhs](https://github.com/charmbracelet/vhs) tapes in this directory, so anyone with vhs installed can reproduce (and improve) them.

## Prerequisites

```bash
brew install vhs                     # macOS
# OR
go install github.com/charmbracelet/vhs@latest

make install                         # puts `nucleus` on PATH
```

You'll also need at least two profiles registered so the demos have something to show. Easiest path:

```bash
nucleus add supabase atlas        # real OAuth — pick a project
nucleus add supabase default      # real OAuth — pick another
```

If you want a stubbed state (no real OAuth) just for recording, edit the tapes to drive the CLI into whatever states you prefer.

## Regenerate

```bash
vhs demo/overview.tape               # → demo/overview.gif
vhs demo/multi-profile.tape          # → demo/multi-profile.gif
```

Commit the resulting `.gif` files alongside the `.tape` scripts.

## Style

- 1200×720 canvas for fullish-terminal GIFs, 820 height when the demo needs extra rows for config files.
- `Catppuccin Mocha` theme — dark, good contrast, matches GitHub's dark-mode README.
- 40ms typing speed reads naturally; faster feels robotic, slower bloats GIF size.

## What lives where

| Tape | Purpose |
|---|---|
| `overview.tape` | README hero. Tour of `connectors`, `list`, `info`, `install --print`. |
| `multi-profile.tape` | The "one connector, many accounts" payoff: two Supabase profiles bound via `.mcp-profiles.toml` with aliases and notes. |
