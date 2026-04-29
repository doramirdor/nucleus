# Demo gif storyboard — Nucleus

The single most important asset in the entire launch. The whole point of the multi-profile architecture is the *fan-out* moment; the gif has to make that moment land in the first 5 seconds, and it has to be readable on a phone screen at HN feed size.

## Tooling

- Recording: **`asciinema rec`** for the terminal portions, exported with **`agg`** to gif (`agg --theme github-dark --font-size 14 demo.cast demo.gif`). Asciinema gives you a tiny gif (≤ 200 KB) and crisp text — never use a real screen recorder for terminal-only content. The output is a *real* gif, not a video, so it autoplays inline on every social platform without sound.
- For Claude Code's UI portion: **`peek`** on Linux or **Kap** on macOS, exported as gif at 12 fps, 1080x720. Run through **`gifsicle -O3 --colors 128`** to get under 5 MB so X / HN / Slack don't transcode it to a slideshow.

## Length budget

- **Total: ≤ 30 seconds.** People scroll past anything longer.
- First 5s carries the punchline. Assume the viewer never sees second 6.

## Frame plan (timestamps in seconds)

### Frames 0:00 – 0:02 — title card

A single static frame with the project name and the one-sentence pitch. No animation. The reader's eye lands on this, parses it, then the gif starts moving.

```
Nucleus
─────────
One MCP connector. Many authenticated accounts.
```

Centered. Black background. Light gray title. Subtitle in 70% opacity. Two seconds is enough — any longer and it feels staged.

### Frames 0:02 – 0:05 — the pain (terminal)

Show the existing pain in 3 seconds. Quick `cat` of a (mocked) MCP config, highlighting that there's exactly one Supabase entry. Voice-over via subtitle:

```
$ cat ~/.claude.json | jq '.mcpServers'
{
  "supabase": { "command": "npx", "args": [...] }
}

# one Supabase. always one Supabase.
```

The comment line is the message. The eye reads it instantly.

### Frames 0:05 – 0:08 — register two profiles

Cut to a clean terminal. Three commands, no waiting:

```
$ nucleus add supabase atlas
✓ atlas — registered (project_id=lcs...)

$ nucleus add supabase staging
✓ staging — registered (project_id=qrs...)

$ nucleus list
ID                   AGE   METADATA
supabase:atlas       3s    project_id=lcs...
supabase:staging     1s    project_id=qrs...
```

Pre-record so the OAuth browser handshake doesn't show up in the gif — that's a different flow and would eat 20s. The audience accepts that auth happened off-screen.

### Frames 0:08 – 0:12 — the moment (the actual punchline)

Cut to Claude Code. Type ONE sentence:

```
> compare the row count of the users table between prod and staging supabase
```

Pause for **one full second** before Claude starts responding — gives the viewer time to register that this is the question they've always wanted to ask. The pause is the comedic timing.

### Frames 0:12 – 0:24 — Claude works

Claude uses `nucleus_find_tool`, sees the `fanout_suggestion`, calls `nucleus_call_plan`. Show:

- the tool call line (collapsed, just the name visible)
- a brief result preview showing **two row counts side by side**

The comparison output is the second punchline. Make sure the numbers are visibly different (e.g. prod 142,883 / staging 47) so the viewer's eye finishes the inference: "yes, the model just queried *both* environments in one shot."

### Frames 0:24 – 0:28 — the receipts

Cut back to the terminal. One command:

```
$ nucleus logs --tool execute_sql -n 2

2026-04-29 15:23:01  OK   supabase:atlas      execute_sql    412ms  nucleus_call_plan
2026-04-29 15:23:01  OK   supabase:staging    execute_sql    389ms  nucleus_call_plan
```

This frame is what makes the engineer-skeptic stop and look. Two parallel calls, audit-logged, with the right `Via` field tag. It's the tweet-bait detail that earns the retweet from someone who's been burned by opaque agent tools.

### Frames 0:28 – 0:30 — outro

Static frame, three seconds:

```
Nucleus
─────────
brew install doramirdor/homebrew-tap/nucleus
github.com/doramirdor/nucleusmcp
```

No music sting, no animation. People take screenshots of outros — make sure the install command is on screen, big.

## Things to NOT do

- **Don't show the OAuth browser flow.** It's slow, and the audience already knows what OAuth looks like.
- **Don't narrate over the gif.** Half the platforms strip audio; the gif has to work silent.
- **Don't show your real Supabase project IDs.** Use stub values. Recording a real gif and forgetting that the project ID is in the prefix is the easiest way to dox a customer database.
- **Don't show errors / retries / "let me try again".** This is a marketing artifact, not a tutorial. The pain frame at 0:02 is the only "things go wrong" moment, and that's about the *old* world, not Nucleus.
- **Don't run the demo at the actual speed of `npm install`.** Pre-record `nucleus add` so it lands in 3 seconds, not 30. People accept this; everybody does it.
- **Don't include a cursor blink in the static frames.** It's distracting and the eye fixates on it instead of the message.

## File outputs

Three formats, same scene, optimized per surface:

- `demo-hn.gif` — 800×500, 30s, ≤ 8 MB. HN's CDN strips anything bigger.
- `demo-x.gif` — 1080×720, 30s, ≤ 15 MB. X gives more headroom.
- `demo-readme.gif` — 1080×720, 30s, ≤ 5 MB. GitHub's gif renderer chokes above this.

All three live at `assets/demo-fanout-{hn,x,readme}.gif` so the README, tweet, and HN comment all link to the right size without re-encoding.

## Pre-flight checklist

- [ ] Real terminal font (not Cascadia Code's ligatures — they render as gibberish in some gif viewers)
- [ ] Dark theme (light theme washes out at HN's compressed display)
- [ ] No trailing-space artifacts in the recorded asciinema file (run `asciinema cat demo.cast | tail`)
- [ ] No real credentials, project IDs, repo names, or email addresses in any frame
- [ ] Final gif file size noted in the LFS attributes if going through GitHub
- [ ] Test play on a phone — if it's not legible at 4 inches wide, reshoot
