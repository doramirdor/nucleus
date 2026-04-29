# Launch checklist — Nucleus v0.2.0

The single source of truth for the day. Work top to bottom; don't skip the order.

## T-7 days: code freeze

- [ ] All v0.2.0 features merged to `main`.
- [ ] CHANGELOG entry for v0.2.0 reads cleanly start-to-finish (do this read aloud — typos hide on screen).
- [ ] `make test` green on macOS + Linux. (CI covers both; spot-check both.)
- [ ] `go test ./... -race` green.
- [ ] `go vet ./...` clean.
- [ ] `golangci-lint run` clean (or document accepted exceptions).
- [ ] `nucleus doctor` PASSes on a clean install (use a throwaway VM or a fresh user account, not your dev machine).
- [ ] `nucleus install` without `claude` CLI on PATH falls back to printing the JSON snippet and the snippet pastes successfully into `~/.claude.json`.
- [ ] `brew install` on a fresh box installs and runs (test with `brew install doramirdor/homebrew-tap/nucleus` from a different machine — your own brew cache lies).

## T-5 days: demo recording

- [ ] Read [demo-gif-storyboard.md](demo-gif-storyboard.md) start to finish. Don't improvise.
- [ ] Record `demo-fanout-readme.gif` (1080×720, ≤ 5 MB, 30 s).
- [ ] Record `demo-fanout-x.gif` (1080×720, ≤ 15 MB, 30 s).
- [ ] Record `demo-fanout-hn.gif` (800×500, ≤ 8 MB, 30 s).
- [ ] Verify each gif:
  - [ ] No real project IDs / repo names / email addresses anywhere on screen.
  - [ ] Plays correctly on a phone (open it on your phone — scroll-friendly width).
  - [ ] Loops cleanly; no abrupt restart artifact.
- [ ] Drop `demo-fanout-readme.gif` into `assets/`, commit, update README intro to point at it.

## T-3 days: launch material

- [ ] Final pass on [hn-post.md](hn-post.md) — title, body, first-comment explainer.
- [ ] Final pass on [tweet-thread.md](tweet-thread.md) — slot the actual gif into tweet 4.
- [ ] Decide which day to launch (Tue/Wed Pacific morning; not Mon, not Fri-pm).
- [ ] Confirm no Anthropic launch / outage / industry event on that day.
- [ ] Make sure the GitHub repo's About section, topics, and social preview are populated:
  - About: `Profile-aware MCP gateway — one connector, many accounts.`
  - Topics: `mcp`, `claude`, `claude-code`, `claude-desktop`, `cursor`, `gateway`, `golang`
  - Social preview image: a still from the demo gif works, sized 1280×640.

## T-1 day: cut the release

- [ ] Bump version in `Makefile` if needed (auto-derived from git tag — usually no manual change).
- [ ] `git tag v0.2.0 && git push origin v0.2.0` on `main`.
- [ ] Verify GoReleaser CI run produced:
  - [ ] macOS arm64 + amd64 binaries
  - [ ] Linux amd64 + arm64 binaries
  - [ ] Windows amd64 binary
  - [ ] Updated Homebrew tap
- [ ] Paste [v0.2.0-release-notes.md](v0.2.0-release-notes.md) into the GitHub release body.
- [ ] Mark "Set as the latest release" — important for the install commands in the README to resolve correctly.
- [ ] Verify `brew install doramirdor/homebrew-tap/nucleus` picks up v0.2.0 (`brew upgrade` if needed).
- [ ] Verify `go install github.com/doramirdor/nucleusmcp/cmd/nucleus@latest` builds the v0.2.0 commit.
- [ ] Run `nucleus doctor` against the freshly-installed binary.

## Launch day: morning of

Time everything in **US Pacific**.

- [ ] **08:30** — final breakfast, water, no coffee yet (jitters help nobody).
- [ ] **08:45** — re-read the HN post body once. Don't edit.
- [ ] **09:00** — submit to HN. Copy the URL the moment it goes live.
- [ ] **09:00:30** — paste the first-comment explainer (already drafted in `hn-post.md`).
- [ ] **09:01** — post the X/Twitter thread with the HN URL in tweet 8.
- [ ] **09:01–10:30** — reply to every HN and X comment. Engagement signals dominate the first 90 minutes; nothing else you do today matters as much.
- [ ] **10:30** — quick status check:
  - HN points ≥ 30 → continue replying, post a Reddit cross-post to `r/ClaudeAI`.
  - HN points < 30 → don't pile on. Reply to ongoing threads, but stop pushing new channels. The HN attempt is over; you can re-pitch in 2 weeks with a different framing.
- [ ] **11:00** — submit PR to `e2b-dev/awesome-mcp-gateways` adding Nucleus. Submit PR to `punkpeye/awesome-mcp-servers` if applicable.
- [ ] **11:30** — drop a one-liner with the GitHub URL in the Anthropic Discord `#show-and-tell` channel.
- [ ] **12:00** — eat. Step away from the screen for 30 minutes.
- [ ] **12:30** — second wave of replies. Triage any GitHub issues that arrived; reply within 4 hours to the first 5 issues even if the fix takes longer.
- [ ] **17:00** — write down 3 sentences of takeaways while it's fresh: what landed, what didn't, what to do differently next time. This goes in a private notes file, not anywhere public.

## Things that look like emergencies but aren't

- **An angry HN comment.** Reply once, calm, factual. Don't re-engage if they spiral.
- **A bug report from a real install.** Acknowledge within an hour, ship a fix within 24h. Bug reports on launch day are *gifts* — they prove people tried it.
- **Anthropic comments / shares.** Don't fanboy in the reply. "Thanks — we share the gateway-roadmap goal" beats "OMG thank you so much."

## Things that *are* emergencies

- **Credential leak in a screenshot.** Pull the gif immediately; rotate the leaked credential; post a correction.
- **A `nucleus install` failure mode someone hits.** This is the single most damaging outcome — first-run failure is the most expensive thing on launch day. Drop everything; ship a hotfix within 4 hours; bump to v0.2.1 and notify in the comments.
- **A security finding.** Don't engage on the public thread. Move to email/DM; coordinate disclosure.

## Don't-do list

- Don't tag Anthropic on X. If they want to share, they will.
- Don't reply to "this is just MetaMCP / Smithery / mcp.so" with anything except a calm pointer to what's actually different (multi-profile UX, fan-out, policy gate). Don't punch down.
- Don't update the HN title or body after submission — HN penalizes edits.
- Don't ask anyone to upvote. The post either has signal or it doesn't.
- Don't post to multiple subreddits the same day. One cross-post is fine; ten is spam.

## Post-launch (T+1, T+7)

- [ ] T+1: triage any reported issues; ship a v0.2.1 patch if needed.
- [ ] T+1: thank-you reply to the most useful HN/X comments. Don't be saccharine.
- [ ] T+7: write a short retrospective post (your blog, a Gist, or a follow-up tweet) — what landed, what surprised you. Even a quiet launch generates valuable signal.
- [ ] T+14: if the launch underperformed, plan the next pitch. Different angle (the policy story? the audit story? the recommender's `because` field?), different week.
