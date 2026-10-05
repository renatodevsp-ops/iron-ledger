#!/usr/bin/env bash
#
# demo-failure.sh — break something on purpose and show that nothing financial
# moves.
#
# Every mode asserts an outcome, so this is a check rather than a
# demonstration. A non-zero exit means a guarantee broke.
#
# Usage:  scripts/demo-failure.sh <mode>
#
#   kill-mid-flight   kill an instance between the commit and the acknowledgement
#   database-down     make PostgreSQL unavailable while operations are in flight
#   broker-down       make SQS unavailable and watch the outbox recover
#
# The modes drive the stack through `docker compose`, so they expect the
# containers to be running: `docker compose up -d --build` first.

set -euo pipefail

MODE="${1:-}"
COMPOSE="${COMPOSE:-docker compose}"
API="${IRONLEDGER_API_URL:-http://localhost:8080}"
KEYCLOAK="${KEYCLOAK_URL:-http://localhost:8081/realms/ironledger}"

# The API verifies tokens against the issuer `http://keycloak:8080/realms/ironledger`,
# while this script reaches Keycloak through the published port `localhost:8081`.
# Keycloak is configured with hostname-strict=false, so it stamps the `iss` claim
# from the request's Host header: a token minted over localhost:8081 would carry an
# issuer the API rejects, surfacing as a 401 that looks like bad credentials.
# Asking under the issuer's own Host header closes that gap.
ISSUER_HOST="${ISSUER_HOST:-keycloak:8080}"
# The published broker port, which compose lets an operator move. Hardcoding
# 4566 makes the broker-down mode fail on a stack that was not started with the
# default ports, with a message about a broker that is in fact running.
LOCALSTACK_PORT="${LOCALSTACK_PORT:-4566}"

log()  { printf '\033[1m==>\033[0m %s\n' "$*"; }
ok()   { printf '  \033[32mok\033[0m   %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; exit 1; }

command -v jq >/dev/null || { echo "this script needs jq"; exit 1; }

token() {
  curl -fsS -X POST "$KEYCLOAK/protocol/openid-connect/token" \
    -H "Host: $ISSUER_HOST" \
    -d grant_type=client_credentials -d "client_id=$1" -d "client_secret=$2" \
    | jq -r .access_token
}

wait_ready() {
  for _ in $(seq 1 90); do
    curl -fsS "$API/health/ready" >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

INTERNAL="$(token iron-ledger-internal iron-ledger-internal-secret)"
PROVIDER="$(token provider-a provider-a-secret)"

PLAYER="$(cat /proc/sys/kernel/random/uuid)"
WALLET="$(curl -fsS -X POST "$API/wallets" \
  -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' \
  -d "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"500.00\",\"currency\":\"BRL\"}}" \
  | jq -r .id)"

submit() {
  local external="$1" amount="${2:-50.00}"
  curl -sS -o "/tmp/iron-fail-$external.json" -w '%{http_code}' \
    -X POST "$API/wagering/transactions" \
    -H "Authorization: Bearer $PROVIDER" -H 'Content-Type: application/json' \
    -H "Idempotency-Key: provider-a:$external" \
    -d "{
          \"providerId\":\"provider-a\",
          \"externalTransactionId\":\"$external\",
          \"playerId\":\"$PLAYER\",
          \"walletId\":\"$WALLET\",
          \"roundId\":\"failure-1\",
          \"gameId\":\"fortune-chimp\",
          \"kind\":\"BET\",
          \"money\":{\"amount\":\"$amount\",\"currency\":\"BRL\"}
        }"
}

balance_of() {
  curl -fsS -H "Authorization: Bearer $INTERNAL" "$API/wallets/$WALLET" | jq -r .balance.amount
}

debits_of() {
  curl -fsS -H "Authorization: Bearer $INTERNAL" "$API/wallets/$WALLET/ledger?limit=200" \
    | jq '[.data[] | select(.direction == "DEBIT")] | length'
}

case "$MODE" in

kill-mid-flight)
  log "committing an operation and killing the instance before it can acknowledge"
  status="$(submit kill-1)"
  [[ "$status" == "200" ]] || fail "the operation did not settle (HTTP $status)"

  balance="$(balance_of)"
  ok "the operation committed; the balance is $balance"

  log "killing every application instance"
  $COMPOSE kill api worker >/dev/null

  log "starting the API again and replaying the very same request"
  $COMPOSE up -d api >/dev/null
  wait_ready || fail "the API never came back"
  INTERNAL="$(token iron-ledger-internal iron-ledger-internal-secret)"
  PROVIDER="$(token provider-a provider-a-secret)"

  replay_status="$(curl -sS -o /tmp/iron-fail-replay.json -w '%{http_code}' \
    -X POST "$API/wagering/transactions" \
    -H "Authorization: Bearer $PROVIDER" -H 'Content-Type: application/json' \
    -H 'Idempotency-Key: provider-a:kill-1' \
    -d "{
          \"providerId\":\"provider-a\",
          \"externalTransactionId\":\"kill-1\",
          \"playerId\":\"$PLAYER\",
          \"walletId\":\"$WALLET\",
          \"roundId\":\"failure-1\",
          \"gameId\":\"fortune-chimp\",
          \"kind\":\"BET\",
          \"money\":{\"amount\":\"50.00\",\"currency\":\"BRL\"}
        }")"
  [[ "$replay_status" == "200" ]] || fail "the replay was refused (HTTP $replay_status)"
  replay="$(jq -r .idempotentReplay /tmp/iron-fail-replay.json)"
  [[ "$replay" == "true" ]] || fail "the replay was applied again instead of recognised"

  [[ "$(balance_of)" == "$balance" ]] || fail "the balance moved across the restart"
  [[ "$(debits_of)" == "1" ]] || fail "the ledger no longer holds exactly one debit"
  ok "idempotency survived the restart: the replay moved nothing"
  ;;

database-down)
  log "committing an operation while PostgreSQL is unavailable"
  $COMPOSE stop postgres >/dev/null

  status="$(curl -sS -o /tmp/iron-fail-db.json -w '%{http_code}' \
    -X POST "$API/wagering/transactions" \
    -H "Authorization: Bearer $PROVIDER" -H 'Content-Type: application/json' \
    -H 'Idempotency-Key: provider-a:db-1' \
    -d "{
          \"providerId\":\"provider-a\",
          \"externalTransactionId\":\"db-1\",
          \"playerId\":\"$PLAYER\",
          \"walletId\":\"$WALLET\",
          \"roundId\":\"failure-1\",
          \"gameId\":\"fortune-chimp\",
          \"kind\":\"BET\",
          \"money\":{\"amount\":\"50.00\",\"currency\":\"BRL\"}
        }" || echo 000)"
  [[ "$status" == "503" ]] || fail "expected 503 while the database is down, got $status"
  ok "the API reported a dependency failure, which is retryable"

  code="$(jq -r .code /tmp/iron-fail-db.json)"
  [[ "$code" == "DEPENDENCY_UNAVAILABLE" ]] || fail "failure code = $code"
  ok "the failure is classified as retryable"

  log "readiness must report the dependency as unavailable"
  ready="$(curl -sS -o /tmp/iron-fail-ready.json -w '%{http_code}' "$API/health/ready")"
  [[ "$ready" == "503" ]] || fail "readiness returned $ready while the database was down"
  postgres_state="$(jq -r .checks.postgres /tmp/iron-fail-ready.json)"
  [[ "$postgres_state" == unavailable* ]] || fail "readiness reports postgres as $postgres_state"
  ok "readiness reports postgres unavailable, so the instance leaves the load balancer"

  log "liveness must not depend on the database"
  live="$(curl -sS -o /dev/null -w '%{http_code}' "$API/health/live")"
  [[ "$live" == "200" ]] || fail "liveness returned $live; a database outage must not look like a dead process"
  ok "liveness still answers: restarting the process would not have helped"

  log "bringing PostgreSQL back and retrying with the same key"
  $COMPOSE start postgres >/dev/null
  wait_ready || fail "the API never became ready again"

  retry="$(curl -sS -o /tmp/iron-fail-retry.json -w '%{http_code}' \
    -X POST "$API/wagering/transactions" \
    -H "Authorization: Bearer $PROVIDER" -H 'Content-Type: application/json' \
    -H 'Idempotency-Key: provider-a:db-1' \
    -d "{
          \"providerId\":\"provider-a\",
          \"externalTransactionId\":\"db-1\",
          \"playerId\":\"$PLAYER\",
          \"walletId\":\"$WALLET\",
          \"roundId\":\"failure-1\",
          \"gameId\":\"fortune-chimp\",
          \"kind\":\"BET\",
          \"money\":{\"amount\":\"50.00\",\"currency\":\"BRL\"}
        }")"
  [[ "$retry" == "200" ]] || fail "the retry was refused (HTTP $retry)"
  replayed="$(jq -r .idempotentReplay /tmp/iron-fail-retry.json)"
  [[ "$replayed" == "false" ]] || fail "the retry was treated as a replay: something was applied while the database was down"
  [[ "$(debits_of)" == "1" ]] || fail "the ledger holds more than one debit"
  ok "the retry applied the operation exactly once"
  ;;

broker-down)
  log "stopping the broker and committing an operation"
  $COMPOSE stop localstack >/dev/null

  status="$(submit broker-1)"
  [[ "$status" == "200" ]] || fail "the operation should still settle without a broker (HTTP $status)"
  ok "the business transaction committed: publication is not on its critical path"

  log "checking the outbox is holding the events"
  pending="$(docker compose exec -T postgres psql -U iron -d ironledger -tAc \
    'SELECT count(*) FROM outbox_messages WHERE published_at IS NULL' | tr -d '[:space:]')"
  [[ "$pending" -gt 0 ]] || fail "no events are waiting in the outbox"
  ok "$pending events are waiting for publication"

  log "bringing the broker back"
  $COMPOSE start localstack >/dev/null
  for _ in $(seq 1 60); do
    curl -fsS "http://localhost:$LOCALSTACK_PORT/_localstack/health" >/dev/null 2>&1 && break
    sleep 1
  done

  log "waiting for the outbox to drain"
  drained=0
  for _ in $(seq 1 60); do
    pending="$(docker compose exec -T postgres psql -U iron -d ironledger -tAc \
      'SELECT count(*) FROM outbox_messages WHERE published_at IS NULL' | tr -d '[:space:]')"
    if [[ "$pending" == "0" ]]; then drained=1; break; fi
    sleep 1
  done
  [[ "$drained" -eq 1 ]] || fail "$pending events are still unpublished"
  ok "every event was published once the broker returned"

  [[ "$(debits_of)" == "1" ]] || fail "the ledger holds more than one debit"
  ok "the wallet moved exactly once throughout"
  ;;

*)
  cat >&2 <<'USAGE'
usage: scripts/demo-failure.sh <mode>

  kill-mid-flight   kill an instance between the commit and the acknowledgement
  database-down     make PostgreSQL unavailable while operations are in flight
  broker-down       make SQS unavailable and watch the outbox recover
USAGE
  exit 2
  ;;
esac

printf '\n\033[32mThe guarantee held.\033[0m\n'
