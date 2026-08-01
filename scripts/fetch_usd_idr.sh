#!/usr/bin/env bash
#
# Fetch the USD → IDR exchange rate and write conversion.md at the repo root.
#
# Scheduled (Windows Task Scheduler) to run daily at 19:00 WITA — Bali time,
# UTC+8, no DST. The local machine runs in the same offset, so no timezone
# conversion is needed for the schedule. Bali time is rendered as UTC+8
# (git-bash has no tz database, so TZ env vars are ignored; +8h from UTC is
# exact because WITA has no DST).
#
# Sources (both free, no API key):
#   primary   https://open.er-api.com/v6/latest/USD  (ExchangeRate-API)
#   fallback  https://api.frankfurter.app            (ECB reference rates)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$REPO_ROOT/conversion.md"

rate=""
source_label=""
api_time=""

# json_field <json> <field> — prints the value of a top-level/rates field.
# Uses python when available, otherwise a sed fallback for simple JSON.
json_field() {
  local json="$1" field="$2"
  if command -v python >/dev/null 2>&1; then
    printf '%s' "$json" | python -c '
import json, sys
try:
    d = json.load(sys.stdin)
    print(d["rates"].get("IDR", ""))
    print(d.get("time_last_update_utc", ""))
except Exception:
    pass
' 2>/dev/null || true
  else
    # Simple regex fallback (frankfurter-style response only).
    printf '%s' "$json" | sed -n 's/.*"IDR":\([0-9.]*\).*/\1/p'
  fi
}

# --- Primary source: ExchangeRate-API -------------------------------------
if resp="$(curl -fsSL --max-time 20 "https://open.er-api.com/v6/latest/USD" 2>/dev/null)"; then
  parsed="$(json_field "$resp" IDR)"
  rate="$(printf '%s\n' "$parsed" | sed -n '1p')"
  api_time="$(printf '%s\n' "$parsed" | sed -n '2p')"
  source_label="ExchangeRate-API — https://open.er-api.com/v6/latest/USD"
fi

# --- Fallback source: Frankfurter (ECB reference rates) -------------------
if [ -z "$rate" ] || [ "$rate" = "None" ]; then
  rate=""
  if resp="$(curl -fsSL --max-time 20 "https://api.frankfurter.app/latest?from=USD&to=IDR" 2>/dev/null)"; then
    parsed="$(json_field "$resp" IDR)"
    rate="$(printf '%s\n' "$parsed" | sed -n '1p')"
    api_time="$(printf '%s\n' "$parsed" | sed -n '2p')"
    source_label="Frankfurter (ECB reference rates) — https://api.frankfurter.app"
  fi
fi

now_bali="$(date -u -d '+8 hours' '+%A, %d %B %Y %H:%M') WITA"

if [ -z "$rate" ] || [ "$rate" = "None" ]; then
  cat > "$OUT" <<EOF
# USD → IDR Exchange Rate

**Error:** could not fetch the exchange rate at $now_bali (both sources failed).

_This file is updated daily at 19:00 WITA (Bali time) by scripts/fetch_usd_idr.sh._
EOF
  echo "ERROR: failed to fetch USD→IDR rate" >&2
  exit 1
fi

rate_fmt="$(printf '%s' "$rate" | python -c 'import sys; print(f"{float(sys.stdin.read().strip()):,.2f}")' 2>/dev/null || printf '%s' "$rate")"

cat > "$OUT" <<EOF
# USD → IDR Exchange Rate

| Field | Value |
|---|---|
| **1 USD** | **$rate_fmt IDR** |
| **API last update** | $api_time |
| **Fetched (Bali time)** | $now_bali |
| **Source** | $source_label |

_Updated daily at 19:00 WITA (Bali time) by scripts/fetch_usd_idr.sh._
EOF

echo "Wrote $OUT: 1 USD = $rate_fmt IDR"
