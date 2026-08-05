# Upstream v1.19.7 upgrade — conflict assessment (fork `main-v2`)

Assessment date: 2026-08-05 · Fork base: `55d92477` (upstream #7125, v1.19.0-preview.2 era) · Upstream target: **v1.19.7** (2026-08-05, 34 commits / 129 files vs v1.19.6)

This is a **written assessment only** — no merge was performed and no source was
changed. It answers: which upstream v1.19.7 changes collide with this fork's
added features (Routines, memory self-improvement loop, markdown chat
rendering), how, and what to watch for when upgrading.

## Executive summary

- **No textual (same-hunk) merge conflicts.** The fork's 6 files that upstream
  v1.19.7 also touched all have the two sides' changes in **disjoint code
  regions** — a `git merge` would resolve cleanly.
- **35 of the fork's 41 changed files are fork-only additions** (new packages
  `internal/routines/`, `internal/memoryreview/`, `internal/headless/`, serve/cli
  additions, docs, scripts) with no upstream counterpart — merge-clean by
  construction.
- **One behavior risk to decide before merging: #7466 makes DeepSeek
  server-side web search default-ON** on official `api.deepseek.com`
  Anthropic/Responses endpoints — a billing/privacy change for this fork, which
  runs DeepSeek by default.
- Three smaller semantic interactions (subagent-result tool, unknown-slash
  turns, unleased-save diagnostics) are benign but worth a glance.

## Method

1. Full change-set: GitHub API `compare/v1.19.6...v1.19.7` → 34 commits, 129
   files with per-file status and ± counts.
2. Fork divergence map: `git diff --name-only 55d92477 HEAD` → 41 files.
3. Overlap set: intersection of the two = **6 files**.
4. For each overlap file, compared the upstream patch hunks against our fork's
   diff hunks (`git diff 55d92477 HEAD -- <file>`), then classified:
   textual / semantic / clean.

## Overlap set (6 files, both sides modified)

| File | Upstream v1.19.7 change (hunk) | Our fork's change (hunk) | Verdict |
| --- | --- | --- | --- |
| `internal/boot/boot.go` | #7375 registers subagent-result tool at ~L909; #7466 `web_search` → `config.EffectiveWebSearch(e)` at ~L2292 | `memoryReviewConfig` + `reviewRunner` + defaults at L1667–1813+ | **Disjoint regions — merges cleanly** |
| `internal/boot/boot_test.go` | #7375 adds `read_subagent_result` to default tool names at ~L2213 | `TestMemoryReviewConfigDefaults` at L55 | **Disjoint — clean** |
| `internal/config/config.go` | #7466 `WebSearch bool` → `*bool` at ~L1349; #7386 provider-isolation | `[routines]`/`[memory]` config structs at L62 + L848–894 | **Disjoint — clean** |
| `internal/control/controller.go` | #7496 unknown slash → regular turn at ~L1315; #7493 evidence-strip refactor at ~L2519 | memory-review nudge fields at L111–516 + `maybeNudgeMemoryReview` at L837–927 | **Disjoint — clean; semantic note below** |
| `internal/i18n/messages_en.go` | #7496/#7450 Slash* message reformat at L154–173 | routines usage line at L561 | **Disjoint — clean** |
| `internal/serve/serve.go` | #7450 `EnableInteractiveApproval` in `switchModel` at ~L222 | `/routines` routes + Server struct at L38–520 | **Disjoint — clean** |

## Semantic risks (behavioral, even where hunks don't collide)

### HIGH — #7466 DeepSeek server-side web search is default-ON (billing)

- New `internal/config/web_search.go`: `EffectiveWebSearch` returns the product
  default (`true`) for **official** `api.deepseek.com` Anthropic/Responses
  endpoints when the user hasn't set `web_search` explicitly.
- `internal/config/load.go` adds `backfillDeepSeekOfficialEndpointDefaults`
  (endpoint-keyed, so it cannot leak onto custom providers — that was the #7357
  bug being fixed).
- `internal/provider/responses/responses.go` now plumbs `web_search` through
  and replays search items (up to 512 KiB each).
- **Impact on this fork:** we run DeepSeek by default; after the upgrade, every
  session on the official endpoint sends queries through server-side search
  unless `web_search = false` is set. Release risk-note says token usage and
  charges may increase. **Decide before merging**: keep the new default (fix
  nothing) or set `web_search = false` in `reasonix.example.toml` / provider
  presets.

### LOW — #7375 subagent-result tool joins the registry

`internal/boot/boot.go` registers `NewSubagentResultTool` and the tool list
gains `read_subagent_result`. This is additive; our `memoryReviewConfig` and
headless runner do not enumerate tools by name, so no interaction. Our
`boot_test.go` will need the new tool name only if a full-tool-name assertion
is added later.

### LOW — #7496 unknown slash input now runs a regular turn

`submitCommandOrTurn` sends unknown `/…` input as a normal message instead of
dead-ending. Interaction with our feature: a tool-using "unknown slash" turn
advances the **memory-review nudge counter** (`lastTurnUsedTools`), which is
correct behavior — just note it means slash-typo turns can contribute toward a
review trigger.

### LOW — #7532 unleased-session-save diagnostics

Adds deduplicated diagnostics for saves without an in-process lease owner; no
behavior change this release. Our memory-review headless runs (`routines` /
`boot.Build`) write transcripts under their own lease, so no interaction —
worth re-checking when upstream's single-writer enforcement lands.

### NONE — #7386 provider-defaults isolation

Endpoint-keyed backfill cannot leak DeepSeek balance URL / pricing / context
window onto custom providers, so our `[routines]`/`[memory]` config additions
and any custom providers are unaffected.

## Clean (no conflict surface)

- **35 fork-only files** — `internal/routines/*` (13), `internal/memoryreview/*`
  (3), `internal/headless/run.go`, `internal/serve/{index.html,
  markdown-it.min.js, routines.go, routines_test.go}`, `internal/cli/{routines.go,
  memorycmd.go}`, `internal/config/edit.go`, `internal/control/memory_review_test.go`,
  `docs/ROUTINES.md`, `docs/SESSION_MEMORY_RETRIEVAL.md`, `AGENTS.md`, `DOCS.md`,
  `reasonix.example.toml`, scripts.
- **94 upstream-only files** — desktop (`desktop/`, ~30 files), CI workflows
  (~18), `internal/provider/*` (stream-stall recovery #7374, responses),
  `internal/bot/gateway.go` (#7531 session_mappings), `internal/agent/*`
  (#7375 parallel results, #7504 planner, #7501 titles), `internal/evidence/*`
  (#7493), `internal/cli/chat_tui.go` (#7489, #4916), `internal/repair/*`
  (#7376 updater), `internal/hook/*` (#7441), `internal/acp/*` (#7502),
  `internal/sysproxy/*` (#7474), release-notes, npm/scripts.

## Recommended merge order (if/when upgrading)

1. Merge `origin/main-v2` state at `ac1abd44` against upstream `v1.19.7` on a
   branch (`git merge` or cherry-pick of the 34 commits — given disjoint hunks,
   `git merge upstream/v1.19.7` should apply without manual conflict
   resolution).
2. **Decide the #7466 default** (`web_search`): set `web_search = false` in
   `reasonix.example.toml` if the DeepSeek billing change is unwanted.
3. `go build ./...`, `go vet ./...`, `go test ./internal/...` — the new
   subagent-result tool and web-search config must compile against our
   `boot.go`/`config.go` edits (disjoint, but the file is shared so the build
   is the real gate).
4. Re-run the fork feature smoke tests: `reasonix routines cron list`, a
   scheduled run, `reasonix memory review --no-apply`, and a serve chat to
   confirm markdown rendering still works after `internal/serve` changes.

## Guardrails

- Re-run this assessment's overlap set after any new upstream release:
  `git diff --name-only <base> HEAD` ∩ `compare/<prev>...<new>` — the 6-file
  set can grow.
- `internal/control/controller.go` is the fork's most load-bearing shared file
  (memory-review nudge) — any upstream change to `finishGuardedTurn`,
  `submitCommandOrTurn`, or the `Controller` struct deserves a focused review.
