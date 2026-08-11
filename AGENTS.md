# AGENTS.md — DeepSeek-Reasonix fork

Go-based Reasonix coding agent fork with a **Routines** subsystem (scheduled
jobs + webhook automations, modeled on Hermes). Entry point `cmd/reasonix`;
build with `make build` → `bin/reasonix.exe`. The single-binary CLI serves
HTTP (`reasonix serve`, default 127.0.0.1:8787), the chat TUI, ACP, a bot
gateway, and now routines.

## Project

- Stack: Go (single static binary, stdlib-only leaf packages), embedded SPA in
  `internal/serve/index.html`.
- Key packages: `internal/routines` (types, schedule parser, JSON store,
  scheduler, headless runner, delivery, webhooks), `internal/serve`
  (HTTP+SSE + the Routines web tab), `internal/cli` (`routines` command
  group), `internal/config` (`[routines]` TOML section), `internal/boot` +
  `internal/control` (headless agent runs).
- Data lives under `<reasonix-home>/routines/`: `jobs.json`,
  `webhook_subscriptions.json`, `deliveries/`, `<job-id>/` output docs,
  `sessions/`.

## Commands

```sh
make build                        # -> bin/reasonix.exe
go test ./internal/routines/...   # scheduler/store/schedule/delivery/webhook tests
go vet ./internal/routines/... ./internal/serve/... ./internal/cli/...
./bin/reasonix.exe serve --addr 127.0.0.1:8788   # web UI incl. Routines tab
```

## How to create a cron job

Two ways — the web **Routines tab** (sidebar of `reasonix serve`, create form
under "Jobs") or the CLI:

```sh
# agent job: the model runs the prompt on schedule
./bin/reasonix.exe routines cron create "0 11 * * *" \
  "Fetch the USD to IDR exchange rate and write conversion.md at the repo root." \
  --name "USD-IDR daily (19:00 Bali)" --deliver local

# no_agent script job: a script IS the job; its stdout is delivered verbatim
./bin/reasonix.exe routines cron create "0 11 * * *" \
  "Fetch USD to IDR and EUR rates, write currencies.md at the repo root." \
  --name "IDR/EUR/USD daily (19:00 Bali)" \
  --script scripts/fetch_currencies.sh --no-agent --deliver local
```

Manage:

```sh
./bin/reasonix.exe routines cron list                # id, name, next run, state, status
./bin/reasonix.exe routines cron show <id>           # full job state incl. last error
./bin/reasonix.exe routines cron run <id>            # dispatch one job now
./bin/reasonix.exe routines cron pause|resume <id>
./bin/reasonix.exe routines cron remove <id>
./bin/reasonix.exe routines start                    # scheduler + webhook receiver service
```

The web tab and CLI share the same store — a job created in one appears in the
other. `reasonix serve` also embeds the scheduler lazily (first `/routines`
call), so the tab can fire jobs while serve runs.

## Schedule formats (accepted by `--schedule` / the web form)

| Input | Meaning |
| --- | --- |
| `30m` / `2h` / `1d` | one-shot at now + duration |
| `every 30m` / `every 2h` | repeating interval |
| `0 2 * * *` | 5-field cron: `minute hour dom month dow` (lists, ranges, `*/n`, `7`=Sunday) |
| `2026-03-02T08:00:00Z` | one-shot at an exact time |

Schedules are evaluated in **UTC**; pick the UTC slot for the desired local
time (e.g. 19:00 WITA Bali = `0 11 * * *`). Human phrases like
`daily at 19:00` are rejected — the error now shows in the UI.

## Delivery targets (`--deliver`)

`local` (default; saves under `<home>/routines/deliveries/<job-id>/`) ·
`origin` · `all` · `platform` · `platform:chat` · `platform:chat:thread`.

Platform delivery reuses the bot adapters (Feishu/QQ/Weixin) from `[bot]`
config; home chats come from `REASONIX_ROUTINES_<PLATFORM>_HOME_CHAT` env vars.

## Webhook subscriptions

```sh
./bin/reasonix.exe routines webhook subscribe pr-review \
  --prompt "Review PR #{pull_request.number}: {pull_request.title}" \
  --events pull_request --deliver feishu:oc_123
./bin/reasonix.exe routines webhook list | remove <slug> | test <slug>
```

POST to `http://127.0.0.1:8644/webhooks/<slug>` with
`X-Hub-Signature-256: sha256=<hmac>` (secret printed at subscribe). Templates
use `{event.field}` dot-notation, `{event_type}`, `{__raw__}`.
`--deliver-only` skips the agent.

## What to clarify with the user before creating a cron job

Ask (or confirm from an existing job) before creating:

- **Schedule & timezone** — the exact cron/interval string, and which timezone
  it should fire in (schedules are UTC; state the local time you're encoding,
  e.g. "daily 19:00 WITA Bali = `0 11 * * *` UTC").
- **Agent vs script** — should the model reason over the task (`--no-agent`
  unset) or is a deterministic script the job (`--no-agent --script …`)?
- **Script path** — where the script lives (relative to the repo root), what
  it needs (network? API key? a running service?), and what its stdout means.
- **Data source & credentials** — which API/endpoint provides the data, and
  whether it needs a key. Prefer no-key sources when possible.
- **Output location** — the exact file to write (e.g. `currencies.md` at the
  repo root) and whether to overwrite or append.
- **Delivery target** — `local`, or a chat (`platform:chat`), and whether
  silence (`[SILENT]`) should suppress notifications.
- **Model** — only needed for agent (non-script) jobs; default is
  `routines.model` → `default_model`.
- **Run now?** — whether to dispatch immediately (`cron run <id>`) or wait for
  the schedule.
- **Repeat limit** — `--repeat N` if the job should stop after N runs.

## Conventions

- This repo is a **fork of upstream Reasonix**. Every feature or
  user-visible change we add must be documented in `DOCS.md` (intro-table row
  + a what/how/use/state section + a `## Commits` row, honest about the
  commit it rides in). Keeping the fork's deltas recorded in one place lets
  you compare against upstream when pulling the latest Reasonix changes and
  spot incompatibilities or merge errors quickly.
- New routines code goes in `internal/routines/` (stdlib-only leaves: store,
  filelock, fileutil — no new deps; schedule parser is hand-rolled).
- The scheduler fires at-most-once: `next_run_at` advances **before**
  dispatch; stale schedules fast-forward (no burst-fire).
- `[SILENT]` in a run's final output suppresses delivery.
- In `internal/serve/index.html`: never name a function parameter `el` — it
  shadows the global `el()` element helper (this bit us once: the Routines
  modal showed "error loading" for any non-empty job list).
- Chat markdown uses the vendored `markdown-it.min.js` (MIT, embedded at
  `internal/serve/markdown-it.min.js`, served at `/assets/markdown-it.min.js`)
  through the `mdRender()` wrapper — `html:false` + link-scheme allowlist;
  never render raw HTML into messages without going through it.
- Webhook secrets are returned once at creation and never in list responses.
- Memory review (`internal/memoryreview` + `[memory]` config): the post-turn
  nudge and the `reasonix memory review` CLI share the same reviewer. It runs
  through the cache-warm headless boot path and never rebuilds a live session's
  system prompt — the prefix-cache invariant is load-bearing, do not "help" by
  injecting review output into the system prompt. `internal/headless` is the
  leaf over `boot.Build`; boot's `reviewRunner` duplicates its shape to avoid a
  `boot → routines → boot` cycle. See `docs/SESSION_MEMORY_RETRIEVAL.md`.

## Notes

- TODO: blueprint catalog, audit ledger, external scheduler providers
  (Hermes-peripheral, deferred).
