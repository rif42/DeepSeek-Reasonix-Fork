# Routines — scheduled jobs & webhook automations

Routines bring Hermes-style automations to Reasonix: **scheduled jobs**
(cron / interval / one-shot) and **webhook triggers** that run the agent
headlessly and deliver the result to a target of your choice. The design
mirrors [Hermes' automations subsystem](https://github.com/NousResearch/hermes-agent):

- Jobs persist as JSON under `<reasonix-home>/routines/` (`jobs.json`,
  `webhook_subscriptions.json`), locked and written atomically.
- A 60s in-process ticker fires due jobs with **at-most-once** semantics: a
  job's `next_run_at` is advanced *before* dispatch, so a crash or restart
  never double-fires it. Stale schedules fast-forward instead of burst-firing.
- Each job runs the agent headlessly (same boot path as `reasonix run`) with
  `auto` tool approval — an unattended routine can never wedge on a prompt no
  one is there to answer.
- Optional pre-run **script**: its stdout is injected into the prompt as
  context. An empty script output skips the agent ("nothing to do"). With
  `--no-agent` the script *is* the job and its stdout is delivered verbatim.
- `[SILENT]` in a run's final response suppresses delivery — you only get
  notified when something actually happened.

## Quick start

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

A scheduled run that delivers to a chat platform:

```bash
reasonix routines cron create "0 9 * * 1" \
  "Generate a weekly AI news digest, keep it under 300 words." \
  --name "Weekly digest" --deliver feishu:oc_123456
```

A script-driven monitor that only notifies on change:

```bash
reasonix routines cron create "every 1h" \
  "If CHANGE DETECTED, summarize what changed. If NO_CHANGE, respond with [SILENT]." \
  --name "Pricing monitor" --script ~/scripts/watch-site.py --deliver local
```

## Schedules

| Input | Meaning |
| --- | --- |
| `30m` / `2h` / `1d` | one-shot, fires once at now + duration |
| `every 30m` / `every 2h` | repeating interval |
| `0 2 * * *` | 5-field cron (`minute hour dom month dow`; lists, ranges, `*/n` steps, `7` = Sunday) |
| `2026-03-02T08:00:00Z` | one-shot at an exact RFC3339 time |

`--repeat N` caps how many times a job runs before it completes.

## Delivery targets

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
`REASONIX_ROUTINES_FEISHU_HOME_CHAT`), or use an explicit
`platform:chat` target.

## Webhooks

Subscribe a route, then point any HTTP client at it (GitHub webhooks, cron
services, alert systems, …):

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
- **Idempotency**: `X-GitHub-Delivery` (or a body hash) dedupes retries for
  1h.
- `--deliver-only`: skip the agent and deliver the rendered payload directly
  (a pure alerting pipe).

A signed test request:

```bash
SECRET=...   # printed by `webhook subscribe`, or store a fixed one with --secret
BODY='{"pull_request":{"number":42,"title":"Fix auth"}}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print "sha256="$2}')
curl -s -X POST http://127.0.0.1:8644/webhooks/pr-review \
  -H "X-GitHub-Event: pull_request" \
  -H "X-Hub-Signature-256: $SIG" \
  -H "X-GitHub-Delivery: $(uuidgen)" \
  -d "$BODY"
```

## CLI reference

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

## Configuration

```toml
[routines]
enabled            = false             # service preference flag
model              = ""                # default model for routine runs (empty = default_model)
max_parallel_jobs  = 4                 # concurrent job cap
webhook_addr       = "127.0.0.1:8644"  # webhook receiver listen address
webhook_rate_limit = 30                # per-route requests per minute
```

## Notes & v1 scope

- Routine transcripts are written to `<reasonix-home>/routines/sessions/`
  (separate from your interactive sessions).
- Job output docs are saved under
  `<reasonix-home>/routines/<job-id>/<timestamp>.md`.
- No blueprint catalog, audit ledger, or external scheduler providers yet —
  those are Hermes-peripheral and can be added later.
