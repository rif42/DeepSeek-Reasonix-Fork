#!/usr/bin/env bash
#
# Fetch USD → IDR and USD → EUR exchange rates and write currencies.md at the
# repo root. Runs daily at 19:00 WITA (Bali time, UTC+8, no DST) as a Reasonix
# routine (no_agent script job).
#
# Sources (both free, no API key):
#   primary   https://open.er-api.com/v6/latest/USD  (ExchangeRate-API)
#   fallback  https://api.frankfurter.app            (ECB reference rates)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$REPO_ROOT/currencies.md"

# rates holds "<pair> <rate> <api_time> <label>" lines for each currency.
rates=""
fetch_failed=0

# fetch_pair <to> — fetches USD -> <to> from the primary source, then the
# fallback; appends a line to $rates on success.
fetch_pair() {
  local to="$1"
  local pair="USD$to"
  local rate="" api_time="" label=""
  if resp="$(curl -fsSL --max-time 20 "https://open.er-api.com/v6/latest/USD" 2>/dev/null)"; then
    parsed="$(printf '%s' "$resp" | python -c '
import json, sys
try:
    d = json.load(sys.stdin)
    print(d["rates"].get(sys.argv[1], ""))
    print(d.get("time_last_update_utc", ""))
except Exception:
    pass
' "$to" 2>/dev/null || true)"
    rate="$(printf '%s\n' "$parsed" | sed -n '1p')"
    api_time="$(printf '%s\n' "$parsed" | sed -n '2p')"
    label="ExchangeRate-API — https://open.er-api.com/v6/latest/USD"
  fi
  if [ -z "$rate" ] || [ "$rate" = "None" ]; then
    rate=""
    if resp="$(curl -fsSL --max-time 20 "https://api.frankfurter.app/latest?from=USD&to=$to" 2>/dev/null)"; then
      parsed="$(printf '%s' "$resp" | python -c '
import json, sys
try:
    d = json.load(sys.stdin)
    print(d["rates"].get(sys.argv[1], ""))
    print(d.get("date", ""))
except Exception:
    pass
' "$to" 2>/dev/null || true)"
      rate="$(printf '%s\n' "$parsed" | sed -n '1p')"
      api_time="$(printf '%s\n' "$parsed" | sed -n '2p')"
      label="Frankfurter (ECB reference rates) — https://api.frankfurter.app"
    fi
  fi
  if [ -z "$rate" ] || [ "$rate" = "None" ]; then
    fetch_failed=1
    return
  fi
  rates="$rates
$pair|$rate|$api_time|$label"
}

fetch_pair IDR
fetch_pair EUR

now_bali="$(date -u -d '+8 hours' '+%A, %d %B %Y %H:%M') WITA"

if [ -n "$rates" ]; then
  cat > "$OUT" <<EOF
# USD Exchange Rates (IDR & EUR)

| Pair | Rate | API last update | Source |
|---|---|---|---|
EOF
  printf '%s\n' "$rates" | while IFS='|' read -r pair rate api_time label; do
    [ -z "$pair" ] && continue
    rate_fmt="$(printf '%s' "$rate" | python -c 'import sys; print(f"{float(sys.stdin.read().strip()):,.4f}")' 2>/dev/null || printf '%s' "$rate")"
    printf '| **1 USD** | **%s %s** | %s | %s |\n' "$rate_fmt" "${pair#USD}" "$api_time" "$label" >> "$OUT"
  done
  cat >> "$OUT" <<EOF

_Fetched at $now_bali by scripts/fetch_currencies.sh._
EOF
  echo "Wrote $OUT with rates:"
  printf '%s\n' "$rates" | sed '/^$/d' | sed 's/|/ /g'
  if [ "$fetch_failed" = "1" ]; then
    echo "WARNING: at least one currency failed; see $OUT for what succeeded." >&2
  fi
  exit 0
fi

cat > "$OUT" <<EOF
# USD Exchange Rates (IDR & EUR)

**Error:** could not fetch any exchange rates at $now_bali (both sources failed).

_Updated daily at 19:00 WITA (Bali time) by scripts/fetch_currencies.sh._
EOF
echo "ERROR: failed to fetch USD rates" >&2
exit 1
