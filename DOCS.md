# DOCS — Features added to the DeepSeek-Reasonix fork

This document describes the features this fork adds on top of upstream
Reasonix. Each section covers **what** the feature does, **how** it works,
**how to use** it (CLI, web UI, config), and **where** its state lives.

| # | Feature | Introduced |
| --- | --- | --- |
| 1 | [Routines — scheduled jobs & webhook automations](#1-routines--scheduled-jobs--webhook-automations) | `e575a063` |
| 2 | [Memory self-improvement loop](#2-memory-self-improvement-loop) | `2594cc0e` → `dd64528c` |
| 3 | [Markdown chat rendering in the serve web UI](#3-markdown-chat-rendering-in-the-serve-web-ui) | `a141b6d9` |
| 4 | [Scrollable session list in the serve web UI sidebar](#4-scrollable-session-list-in-the-serve-web-ui-sidebar) | `c209661a` (bundled) |

Features 1–3 were ported/inspired by the
[Hermes agent](https://github.com/NousResearch/hermes-agent) and are documented
per-topic under [`docs/`](docs/): `docs/ROUTINES.md` and
`docs/SESSION_MEMORY_RETRIEVAL.md` hold the deeper detail.

---

## 1. Routines — scheduled jobs & webhook automations

Routines bring Hermes-style automations to Reasonix: **scheduled jobs**
(cron / interval / one-shot) and **webhook triggers** that run the agent
headlessly and deliver the result to a target of your choice.

### What it does

- **Scheduled jobs** fire the agent on a schedule and save/deliver the result.
- **Webhook subscriptions** expose an HTTP endpoint that runs the agent when a
  signed request arrives (GitHub webhooks, cron services, alert systems, …).
- **Delivery** routes each run's result to `local` disk or a chat platform
  (Feishu / QQ / Weixin) through the existing bot-gateway adapters.
- **Script pre-injection**: a job can run a script first and feed its stdout
  into the prompt; with `--no-agent` the script *is* the job.
- **`[SILENT]`** in a run's final response suppresses delivery — you only get
  notified when something actually happened.

### How it works

- Jobs persist as JSON under `<reasonix-home>/routines/` (`jobs.json`,
  `webhook_subscriptions.json`), locked and written atomically.
- A **60s in-process ticker** fires due jobs with **at-most-once** semantics: a
  job's `next_run_at` is advanced *before* dispatch, so a crash or restart
  never double-fires it. Stale schedules fast-forward instead of burst-firing.
- Each job runs the agent headlessly (same boot path as `reasonix run`) with
  `auto` tool approval — an unattended routine can never wedge on a prompt no
  one is there to answer.
- Schedules are evaluated in **UTC**; pick the UTC slot for the desired local
  time (e.g. 19:00 WITA Bali = `0 11 * * *`).

### Quick start

```bash
# 1. Create a job: every 30 minutes, run the agent and save the result locally.
reasonix routines cron create "every 30m" \
  "Check the CI status and summarize any failures." \
  --name "CI watch" --deliver local

# 2. Run the service (scheduler + webhook receiver) — leave this running.
reasonix routines start

# 3. See it fire.
reasonix routines cron list
```

A script-driven monitor that only notifies on change:

```bash
reasonix routines cron create "every 1h" \
  "If CHANGE DETECTED, summarize what changed. If NO_CHANGE, respond with [SILENT]." \
  --name "Pricing monitor" --script ~/scripts/watch-site.py --deliver local
```

### Schedules

| Input | Meaning |
| --- | --- |
| `30m` / `2h` / `1d` | one-shot, fires once at now + duration |
| `every 30m` / `every 2h` | repeating interval |
| `0 2 * * *` | 5-field cron (`minute hour dom month dow`; lists, ranges, `*/n` steps, `7` = Sunday) |
| `2026-03-02T08:00:00Z` | one-shot at an exact RFC3339 time |

`--repeat N` caps how many times a job runs before it completes.

### Delivery targets

| Target | Meaning |
| --- | --- |
| `local` (default) | save to `<reasonix-home>/routines/deliveries/<job-id>/` |
| `origin` | the chat the job was created from (falls back to `local` if unknown) |
| `platform` | that platform's home chat |
| `platform:chat` | explicit chat |
| `platform:chat:thread` | explicit chat + thread (thread is reserved; ignored by v1 adapters) |
| `all` | every configured platform's home chat |

Platform delivery reuses the **bot gateway adapters** (Feishu / QQ / Weixin):
`routines start` wires delivery adapters from your `[bot]` connections. Home
chats come from `REASONIX_ROUTINES_<PLATFORM>_HOME_CHAT` (e.g.
`REASONIX_ROUTINES_FEISHU_HOME_CHAT`), or use an explicit `platform:chat`
target.

### Webhooks

```bash
reasonix routines webhook subscribe pr-review \
  --prompt "Review PR #{pull_request.number}: {pull_request.title} by {pull_request.user.login}." \
  --events "pull_request" \
  --deliver feishu:oc_123
```

- Route: `POST /webhooks/<slug>` on the webhook listener
  (default `127.0.0.1:8644`; `webhook_addr` / `--webhook-addr` to change).
- **Auth**: each subscription has an HMAC secret. Verify with the
  `X-Hub-Signature-256: sha256=<hex>` header (GitHub-style) or
  `X-Reasonix-Signature`. A subscription with an empty secret accepts
  unsigned requests (dev mode).
- **Templates**: `{event.field}` dot-notation into the JSON payload,
  `{event_type}` (from `X-GitHub-Event` / `X-Event-Key` / `action`), and
  `{__raw__}` for the full payload.
- **Event filter**: `--events "pull_request,issues"` ignores everything else.
- **Idempotency**: `X-GitHub-Delivery` (or a body hash) dedupes retries for 1h.
- `--deliver-only`: skip the agent and deliver the rendered payload directly
  (a pure alerting pipe).

### Web tab in `reasonix serve`

`reasonix serve` embeds a lazily-initialized routines service (store +
scheduler + local delivery), created on the **first** routines API call so a
serve process without routines config stays untouched. The **Routines tab**
(sidebar) lists jobs and webhooks, creates jobs from a form, and can fire
(`run`), pause, resume, and remove them. The web tab and CLI share the same
store — a job created in one appears in the other.

HTTP API (same-origin, inherits serve auth):

```
GET  /routines                          list jobs + subscriptions (redacted)
POST /routines/jobs                     create a job
POST /routines/jobs/{id}/pause|resume|remove|run
POST /routines/webhooks                 create a webhook subscription
POST /routines/webhooks/{slug}/remove
```

### CLI reference

```
reasonix routines start [--dir DIR] [--model MODEL] [--webhook-addr ADDR] [--no-webhook]
reasonix routines cron create <schedule> <prompt> [--name N] [--model M] [--deliver T]
                        [--script PATH] [--no-agent] [--repeat N] [--workdir DIR]
reasonix routines cron list | show <id> | pause <id> | resume <id> | remove <id>
reasonix routines cron run <id>        # dispatch one job now
reasonix routines cron tick            # dispatch every due job now
reasonix routines webhook subscribe <slug> --prompt P [--events E] [--secret S]
                        [--deliver T] [--deliver-only] [--description D]
reasonix routines webhook list | remove <slug> | test <slug>
```

### Configuration

```toml
[routines]
enabled            = false             # service preference flag
model              = ""                # default model for routine runs (empty = default_model)
max_parallel_jobs  = 4                 # concurrent job cap
webhook_addr       = "127.0.0.1:8644"  # webhook receiver listen address
webhook_rate_limit = 30                # per-route requests per minute
```

### State location

- Job/subscription JSON: `<reasonix-home>/routines/jobs.json`,
  `webhook_subscriptions.json`
- Routine transcripts: `<reasonix-home>/routines/sessions/` (separate from
  interactive sessions)
- Job output docs: `<reasonix-home>/routines/<job-id>/<timestamp>.md`
- Local deliveries: `<reasonix-home>/routines/deliveries/<job-id>/`

### Scope & limitations

- No blueprint catalog, audit ledger, or external scheduler providers yet —
  those are Hermes-peripheral and deferred.

---

## 2. Memory self-improvement loop

A Hermes-style closed loop on top of Reasonix's existing auto-memory store:
after **tool-heavy turns**, a detached LLM pass reads the session transcript,
distills durable facts, and writes them to the same memory store the `remember`
tool uses — no user prompt or interactive approval required.

### What it does

- **Nudge**: after each turn that used at least one tool call, a counter
  advances; at the interval a review is spawned on a detached goroutine.
- **Review**: the reviewer (`internal/memoryreview`) loads the live session
  transcript, renders it compactly, and asks the model for JSON memory facts
  alongside the list of existing facts so it avoids near-duplicates.
- **Apply**: facts are persisted via `memory.Store.SaveWithOptions` — matched
  name + identical content is **skipped**, matched + changed is **updated**
  (revision bump), otherwise **created** (`RequireCreate`, project scope by
  default). Invalid facts (empty name/body) are skipped; a disabled store is a
  no-op.

### How it works (the loop)

1. **Nudge** — `finishGuardedTurn` → `maybeNudgeMemoryReview`
   (`internal/control/controller.go`). Only tool-using turns count; the gate
   requires `review_min_turns` user turns first. **At most one review runs at a
   time**; the nudge never blocks the turn that triggered it.
2. **Review run** — the reviewer renders the transcript (system messages
   dropped, tool results truncated to a budget) and asks the model for facts of
   the shape `{type, scope, name, title, description, body}`, listing existing
   facts so it can reuse `name` to update rather than duplicate.
3. **Apply** — `memoryreview.Reviewer.Apply` persists via
   `memory.Store.SaveWithOptions` (created/updated/skipped per above).

The review's LLM call runs through the standard headless boot path
(`boot.Build`), so it shares the **byte-identical system-prompt prefix** with
interactive sessions → prompt-cache warmth. It never rebuilds any live
session's system prompt, and same-session visibility of new facts stays on the
existing `<memory-update>` tail injection (`memory.Queue`).

### Configuration (`reasonix.toml`)

```toml
[memory]
review_enabled              = true    # set false to disable entirely
review_nudge_interval       = 10      # tool-using turns between reviews (0/absent = default 10)
review_model                = ""      # review model (empty = default_model)
review_min_turns            = 4       # minimum user turns before a review may fire (0/absent = default 4)
review_max_transcript_chars = 40000   # budget guard on the transcript fed to the review
```

> **Defaults note:** `review_nudge_interval` and `review_min_turns` fall back
> to `10` / `4` when absent (applied in `boot.memoryReviewConfig`). To disable
> the loop entirely set `review_enabled = false`; `0` no longer means
> "disabled". `review_max_transcript_chars` falls back to `40000` in
> `memoryreview.Reviewer`.

### On-demand trigger

```sh
# Review the most recent session in the CLI session dir (applies facts):
reasonix memory review

# Review a specific session file:
reasonix memory review --session ~/.reasonix/sessions/2026-08-01/abc.jsonl

# Dry run: distill and print facts without writing anything:
reasonix memory review --no-apply
```

The interactive nudge and the CLI trigger share the same reviewer, so
`--no-apply` is a safe way to preview what the auto-review would write.

### Design notes

- The review runs with `MaxSteps: 1` and its prompt instructs the model not to
  call tools, keeping the unattended pass side-effect free.
- The review model is instructed to never silently change an existing fact's
  scope; `Apply` enforces this (updates keep the stored scope).
- `internal/headless` is a leaf over `boot.Build`; boot's own `reviewRunner`
  duplicates its ~15-line shape so `boot` can satisfy `memoryreview.Runner`
  without a `boot → routines → boot` import cycle.
- Full design + cache-safety invariants: `docs/SESSION_MEMORY_RETRIEVAL.md`.

---

## 3. Markdown chat rendering in the serve web UI

The `reasonix serve` web UI renders chat messages (user + assistant) as
Markdown instead of raw text.

### What it does

- User messages and assistant streaming text are rendered through a single
  pinned `markdown-it` instance.
- **Security**: raw HTML from model/user text is escaped (`html:false`), and
  link targets are restricted to `http/https/mailto`, relative paths, and `#`
  anchors — `javascript:` etc. never make it into `hrefs` (custom
  `validateLink`).
- **Streaming**: while a response streams, live re-render is skipped above
  `MD_LIVE_RENDER_CAP` (50 000 bytes); the final message is always re-rendered
  once complete.
- **Fallback**: if the vendored asset hasn't loaded, `mdRender` falls back to
  escaped text.

### How it works

- The vendored `markdown-it.min.js` (MIT) is served at
  `/assets/markdown-it.min.js` and loaded by `internal/serve/index.html`.
- The `mdRender(src)` wrapper (`internal/serve/index.html`) pins one
  `markdown-it` instance with `html: false`, `linkify: false`,
  `breaks: false`, and the link-scheme allowlist above.
- Every message bubble carries the `msg__text--md` class and is filled with
  `mdRender(...)` output — both `addUserMsg` and the assistant streaming path
  (`appendText` / `finalizeMsg`).

### Scope

- Rendering is client-side in the serve web UI only; the CLI TUI and desktop
  shell have their own rendering paths and are unaffected.
- Never render raw HTML into messages without going through `mdRender`.

---

## 4. Scrollable session list in the serve web UI sidebar

The `reasonix serve` sidebar session list scrolls independently instead of
squashing its items to fit.

### What it does

- With many sessions, the list pane (`#session-list`) scrolls on its own while
  the brand header and the status footer stay pinned.
- Every session item keeps its full height (~47px: title + meta + padding)
  instead of being compressed into a sliver so the whole list can fit.

### How it works

- The sidebar is a fixed-height flex column: `.app` is a `100vh` grid and
  `.sidebar` has `overflow:hidden`, so the sidebar can never grow past the
  viewport.
- `.session-list` and `.sidebar__nav` are `flex:1` scroll children. They need
  `min-height:0` so the flex container is allowed to shrink them below their
  content height — without it the list grew to its content size and the
  sidebar's `overflow:hidden` clipped it instead of scrolling.
- `.session-item` additionally needs `flex-shrink:0`: its `overflow:hidden`
  (used for title ellipsis) disables the flexbox automatic minimum size, which
  let the items compress to ~16px so the list never overflowed. With
  `flex-shrink:0` items keep their natural height and the list overflows →
  scrolls.

### Scope

- CSS-only change in `internal/serve/index.html` (the embedded SPA); no Go
  logic or state involved.
- Affects the serve web UI sidebar only; the CLI TUI resume picker renders its
  own session list.

---

## Commits

| Feature | Commits |
| --- | --- |
| Routines | `e575a063` (scheduled jobs, webhook automations, serve tab + currency job) |
| Memory loop | `2594cc0e` (nudge + config) → `600fb25a` (background reviewer) → `51a6d880` (CLI + boot wiring + docs) → `dd64528c` (merge) |
| Markdown rendering | `a141b6d9` (render chat messages as markdown via vendored markdown-it) |
| Scrollable session list | bundled in `c209661a` (multi-instance tabs; no dedicated commit) |
