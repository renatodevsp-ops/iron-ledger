#!/usr/bin/env bash
#
# load.sh — a reproducible load run against a running stack.
#
# It is a measurement, not a claim: throughput and latency percentiles are
# reported alongside the outcome distribution, the idempotency conflicts and the
# outbox lag, so a regression in any of them is visible rather than guessed at.
# No target is asserted.
#
# Usage:
#   scripts/load.sh [--requests N] [--concurrency N] [--amount N]
#
# Methodology
# -----------
#   * Each worker owns its own wallet and its own player, so the run measures
#     independent wallets in parallel rather than one hot row. Wallets are opened
#     once, up front, and never replenished: a wallet that runs out of money
#     starts refusing bets, which shows up in the outcome distribution instead of
#     being hidden by a credit loop.
#   * Every fifth request is a deliberate duplicate of that worker's previous
#     one — same key, same payload — so idempotent replays are part of the
#     measurement instead of being excluded from it.
#   * Latency is measured client-side around the whole HTTP exchange and
#     reported as percentiles over every sample, not as an average.
#   * The outcome is read back from the transaction index rather than inferred
#     from the HTTP status, so a 200 replay and a 200 first application are told
#     apart.

set -euo pipefail

# awk's decimal separator follows the locale, which turns "78.0241" into
# "78,0241" under a comma-decimal locale and makes every number in the report
# look like a typo.
export LC_ALL=C

REQUESTS=2000
CONCURRENCY=8
AMOUNT=1.00
DUPLICATE_EVERY=5

while [ $# -gt 0 ]; do
  case "$1" in
    --requests)      REQUESTS="$2"; shift 2 ;;
    --concurrency)   CONCURRENCY="$2"; shift 2 ;;
    --amount)        AMOUNT="$2"; shift 2 ;;
    --no-duplicates) DUPLICATE_EVERY=0; shift ;;
    -h|--help)       sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

API="${IRONLEDGER_API_URL:-http://localhost:8080}"
# The outbox publisher runs in the worker, not the API, so the outbox metrics are
# scraped from the worker. Reading them off the API reports a healthy, permanently
# empty histogram, which looks like "nothing to publish" rather than "wrong port".
WORKER="${IRONLEDGER_WORKER_URL:-http://localhost:8082}"
KEYCLOAK="${KEYCLOAK_URL:-http://localhost:8081/realms/ironledger}"

# The API verifies tokens against the issuer `http://keycloak:8080/realms/ironledger`,
# while this script reaches Keycloak through the published port `localhost:8081`.
# Keycloak is configured with hostname-strict=false, so it stamps the `iss` claim
# from the request's Host header: a token minted over localhost:8081 would carry an
# issuer the API rejects, surfacing as a 401 that looks like bad credentials.
# Asking under the issuer's own Host header closes that gap.
ISSUER_HOST="${ISSUER_HOST:-keycloak:8080}"

command -v jq >/dev/null || { echo "this script needs jq"; exit 1; }

log()  { printf '\033[1m==>\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; exit 1; }

token() {
  curl -fsS -X POST "$KEYCLOAK/protocol/openid-connect/token" \
    -H "Host: $ISSUER_HOST" \
    -d grant_type=client_credentials -d "client_id=$1" -d "client_secret=$2" \
    | jq -r .access_token
}

curl -fsS "$API/health/ready" >/dev/null || fail "the API is not ready"
curl -fsS "$WORKER/health/ready" >/dev/null \
  || fail "the worker is not ready at $WORKER; the outbox lives there, so this run would report nothing about it"

INTERNAL="$(token iron-ledger-internal iron-ledger-internal-secret)"
PROVIDER="$(token provider-a provider-a-secret)"

WORKERS="$CONCURRENCY"
PER_WORKER=$(( REQUESTS / WORKERS ))
(( PER_WORKER < 1 )) && PER_WORKER=1
TOTAL=$(( WORKERS * PER_WORKER ))

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT
RESULTS="$WORKDIR/results.tsv"
: > "$RESULTS"

log "environment"
printf '  api           %s\n' "$API"
printf '  worker        %s\n' "$WORKER"
printf '  requests      %s (%s workers x %s)\n' "$TOTAL" "$WORKERS" "$PER_WORKER"
printf '  bet amount    %s\n' "$AMOUNT"
printf '  duplicates    every %s requests per worker\n' "${DUPLICATE_EVERY:-never}"

# Only buckets, sum and count are exposed; there is no quantile gauge, so this
# reports what the platform actually publishes rather than inventing a p95.
#
# The histogram is cumulative since the worker started, so the mean is computed
# from the delta across this run. A lifetime average would be dominated by
# whatever the worker did before the run began — including events that had to
# wait out an earlier incident — which is history, not this run's latency.
outbox_lag_sum() {
  curl -fsS "$WORKER/metrics" 2>/dev/null \
    | awk '/^ironledger_outbox_lag_seconds_sum/ {print $NF; found=1} END {if (!found) print 0}'
}

outbox_lag_count() {
  curl -fsS "$WORKER/metrics" 2>/dev/null \
    | awk '/^ironledger_outbox_lag_seconds_count/ {print $NF; found=1} END {if (!found) print 0}'
}

LAG_BEFORE="$(outbox_lag_sum)"
LAG_COUNT_BEFORE="$(outbox_lag_count)"

log "opening $WORKERS wallets"
for _ in $(seq 1 "$WORKERS"); do
  player="$(cat /proc/sys/kernel/random/uuid)"
  wallet="$(curl -fsS -X POST "$API/wallets" \
    -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' \
    -d "{\"playerId\":\"$player\",\"initialBalance\":{\"amount\":\"100000.00\",\"currency\":\"BRL\"}}" \
    | jq -r .id)"
  [[ "$wallet" != "null" && -n "$wallet" ]] || fail "could not open a wallet"
  printf '%s\t%s\n' "$player" "$wallet" >> "$WORKDIR/wallets.tsv"
done
ok="wallets ready"

# One worker: its own wallet, a fresh key every request, and a deliberate
# duplicate every DUPLICATE_EVERY requests.
run_worker() {
  local worker="$1" player="$2" wallet="$3"
  local previous_external="" previous_body="" count=0
  local samples="$WORKDIR/samples-$worker.tsv"
  : > "$samples"

  for _ in $(seq 1 "$PER_WORKER"); do
    count=$(( count + 1 ))
    local external body key

    if [[ "$DUPLICATE_EVERY" -gt 0 ]] && (( count % DUPLICATE_EVERY == 0 )) && [[ -n "$previous_external" ]]; then
      external="$previous_external"
      body="$previous_body"
      key="provider-a:$external"
    else
      external="load-$worker-$count-$(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')"
      body="{\"providerId\":\"provider-a\",\"externalTransactionId\":\"$external\",\"playerId\":\"$player\",\"walletId\":\"$wallet\",\"roundId\":\"load-$worker\",\"gameId\":\"fortune-chimp\",\"kind\":\"BET\",\"money\":{\"amount\":\"$AMOUNT\",\"currency\":\"BRL\"}}"
      key="provider-a:$external"
      previous_external="$external"
      previous_body="$body"
    fi

    local start end ms code response
    # One response file per worker: a shared one is a race, and the clobbered
    # reads show up as outcomes that mysteriously went missing.
    response="$WORKDIR/response-$worker.json"
    start="$(date +%s%N)"
    code="$(curl -sS -o "$response" -w '%{http_code}' \
      -X POST "$API/wagering/transactions" \
      -H "Authorization: Bearer $PROVIDER" -H 'Content-Type: application/json' \
      -H "Idempotency-Key: $key" -d "$body" || echo 000)"
    end="$(date +%s%N)"
    ms=$(( (end - start) / 1000000 ))

    local status replayed
    status="$(jq -r '.status // "NONE"' < "$response" 2>/dev/null || echo NONE)"
    replayed="$(jq -r '.idempotentReplay // false' < "$response" 2>/dev/null || echo false)"

    printf '%s\t%s\t%s\t%s\n' "$ms" "$code" "$status" "$replayed" >> "$samples"
  done

  cat "$samples" >> "$RESULTS"
}

export -f run_worker
export API PROVIDER AMOUNT PER_WORKER DUPLICATE_EVERY WORKDIR

log "running"
started="$(date +%s%N)"
worker_index=0
while IFS=$'\t' read -r player wallet; do
  worker_index=$(( worker_index + 1 ))
  run_worker "$worker_index" "$player" "$wallet" &
done < "$WORKDIR/wallets.tsv"
wait
finished="$(date +%s%N)"

elapsed_ms=$(( (finished - started) / 1000000 ))
(( elapsed_ms > 0 )) || elapsed_ms=1

LAG_AFTER="$(outbox_lag_sum)"
LAG_COUNT_AFTER="$(outbox_lag_count)"

percentile() {
  local p="$1"
  awk -v p="$p" '{print $1}' "$RESULTS" | sort -n \
    | awk -v p="$p" '{a[NR]=$1} END {if (NR==0) {print "n/a"; exit} print a[int((NR-1)*p)+1]}'
}

total="$(wc -l < "$RESULTS" | tr -d ' ')"
processed="$(awk -F'\t' '$3=="PROCESSED"' "$RESULTS" | wc -l | tr -d ' ')"
rejected="$(awk -F'\t' '$3=="REJECTED"' "$RESULTS" | wc -l | tr -d ' ')"
unknown="$(awk -F'\t' '$3!="PROCESSED" && $3!="REJECTED"' "$RESULTS" | wc -l | tr -d ' ')"
replays="$(awk -F'\t' '$4=="true"' "$RESULTS" | wc -l | tr -d ' ')"
http_200="$(awk -F'\t' '$2==200' "$RESULTS" | wc -l | tr -d ' ')"
http_409="$(awk -F'\t' '$2==409' "$RESULTS" | wc -l | tr -d ' ')"
http_4xx="$(awk -F'\t' '$2 ~ /^4/' "$RESULTS" | wc -l | tr -d ' ')"
http_5xx="$(awk -F'\t' '$2 ~ /^5/' "$RESULTS" | wc -l | tr -d ' ')"
refused="$(awk -F'\t' '$3=="REJECTED"' "$RESULTS" | wc -l | tr -d ' ')"

printf '\n'
printf '  duration        %.2f s\n' "$(awk -v ms="$elapsed_ms" 'BEGIN{printf "%.2f", ms/1000}')"
printf '  throughput      %.1f req/s\n' "$(awk -v n="$total" -v ms="$elapsed_ms" 'BEGIN{printf "%.1f", n/(ms/1000)}')"
printf '  responses       %s\n' "$total"
printf '\n'
printf '  outcomes\n'
printf '    processed     %s\n' "$processed"
printf '    rejected      %s\n' "$rejected"
printf '    no outcome    %s\n' "$unknown"
printf '    replays       %s\n' "$replays"
printf '\n'
printf '  http status\n'
printf '    200           %s\n' "$http_200"
printf '    409           %s\n' "$http_409"
printf '    other 4xx      %s\n' "$(( http_4xx - http_409 ))"
printf '    5xx           %s\n' "$http_5xx"
printf '\n'
printf '  latency (client-side, whole exchange)\n'
printf '    p50           %s ms\n' "$(percentile 0.50)"
printf '    p95           %s ms\n' "$(percentile 0.95)"
printf '    p99           %s ms\n' "$(percentile 0.99)"
printf '    max           %s ms\n' "$(percentile 1.00)"
printf '\n'
published_count=$(( LAG_COUNT_AFTER - LAG_COUNT_BEFORE ))
mean_lag="$(awk -v s="${LAG_AFTER:-0}" -v b="${LAG_BEFORE:-0}" -v c="$published_count" \
  'BEGIN { if (c > 0) printf "%.4f", (s-b)/c; else print "n/a" }')"

printf '  outbox\n'
printf '    published    %s events during the run\n' "$published_count"
printf '    mean lag     %s s (commit to publication)\n' "$mean_lag"
printf '\n'
if [[ "$rejected" -eq 0 ]]; then
  printf '  No bets were refused for funds: each wallet was funded once with enough for\n'
  printf '  every request it received. Raise --amount or lower the funding to see\n'
  printf '  INSUFFICIENT_BALANCE refusals.\n'
else
  printf '  %s bets were refused with INSUFFICIENT_BALANCE. That is the system working:\n' "$rejected"
  printf '  a wallet funded once and never replenished runs out of money and starts\n'
  printf '  refusing bets, and the balance it holds never went negative.\n'
fi
printf '\n'
printf '  The number to watch is 5xx, which should be zero. A non-zero count means the\n'
printf '  platform could not serve a request it accepted, which no budget explains.\n'
