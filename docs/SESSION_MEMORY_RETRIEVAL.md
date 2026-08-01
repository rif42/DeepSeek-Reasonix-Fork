# Session memory & the background review loop

Reasonix auto-memory (`memory.Store` — the `remember` tool, `[memory]` docs)
keeps durable facts per project. The **background memory review** adds a
Hermes-style self-improvement loop: after tool-heavy turns, a detached LLM pass
reads the session transcript, distills consolidated facts, and writes them to
the same store — without any user prompt or interactive approval.

## How it works

1. **Nudge** — after each turn that used at least one tool call
   (`finishGuardedTurn` → `maybeNudgeMemoryReview`), a counter advances. When it
   reaches `review_nudge_interval` (default 10) — and the session has at least
   `review_min_turns` user turns (default 4) — the controller spawns the review
   on a **detached goroutine**. At most one review runs at a time; the nudge
   never blocks or slows the turn that triggered it.
2. **Review run** — the reviewer (`internal/memoryreview`) loads the live
   session transcript (`agent.LoadSession`), renders it compactly (system
   messages dropped, tool results truncated, `review_max_transcript_chars`
   budget), and asks the model for JSON memory facts
   (`{type, scope, name, title, description, body}`) alongside the list of
   existing facts so it can avoid near-duplicates and prefer updates.
3. **Apply** — `memoryreview.Reviewer.Apply` persists via
   `memory.Store.SaveWithOptions`:
   - name matches an existing fact, identical content → **skipped**
   - name matches, content changed → **updated** (revision bump, existing scope
     preserved)
   - otherwise → **created** (`RequireCreate`, project scope by default)
   - invalid facts (empty name/body) → skipped; a disabled store is a no-op.

## Cache-hit-rate safety (the hard invariant)

The fork is tuned around DeepSeek's prefix cache: the session's system prompt
(instructions + AGENTS/REASONIX hierarchy + auto-memory index) is composed once
per session and **never rebuilt mid-session**. The review loop preserves this:

- The nudge only writes to the store; it never touches the live system prompt.
- The review's own LLM call runs through the standard headless boot path
  (`boot.Build` — the same assembly as `reasonix run`), so it shares the
  **byte-identical system-prompt prefix** with interactive sessions →
  prompt-cache warmth on the review call itself.
- Same-session visibility of new facts stays on the existing `<memory-update>`
  tail injection (`memory.Queue`) — the tail, never the prefix.
- Index entries stay compact (name + one-line description), so any next-session
  re-warm after a review write is a small, one-time partial miss — exactly the
  cost a manual `remember` already pays.

## Configuration (`reasonix.toml`)

```toml
[memory]
review_enabled            = true    # set false to disable entirely
review_nudge_interval     = 10      # tool-using turns between reviews (0 = disabled)
review_model              = ""      # review model (empty = default_model)
review_min_turns          = 4       # minimum user turns before a review may fire
review_max_transcript_chars = 40000 # budget guard on the transcript
```

## On-demand trigger

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

## Design notes

- `internal/headless` is a leaf over `boot.Build` used by the routines engine;
  boot's own `reviewRunner` duplicates its ~15-line shape so `boot` can satisfy
  `memoryreview.Runner` without a `boot → routines → boot` import cycle.
- The review runs with `MaxSteps: 1` and its prompt instructs the model not to
  call tools, keeping the unattended pass side-effect free.
- The review model is instructed to never silently change an existing fact's
  scope; `Apply` enforces this (updates keep the stored scope).
