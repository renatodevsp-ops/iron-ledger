#!/usr/bin/env bash
#
# demo-race.sh — the mandatory race, end to end, against a running stack.
#
# A wallet holding 100.00 BRL receives two distinct bets of 80.00 BRL at the same
# instant. Exactly one must be processed, one must be refused for insufficient
# funds, the final balance must be 20.00 and the ledger must hold one debit.
#
# The script asserts every one of those outcomes, so it is a check rather than a
# demonstration: a non-zero exit means a guarantee broke.
#
# Usage:  scripts/demo-race.sh [api-url]

set -euo pipefail

API="${1:-${IRONLEDGER_API_URL:-http://localhost:8080}}"
KEYCLOAK="${KEYCLOAK_URL:-http://localhost:8081/realms/ironledger}"
# The API runs inside the compose network and verifies tokens against the issuer
# `http://keycloak:8080/realms/ironledger`, while this script reaches Keycloak
# through the published port `localhost:8081`. Keycloak is configured with
# hostname-strict=false, so it stamps the `iss` claim from the request's Host
# header — meaning a token minted over localhost:8081 would carry an issuer the
# API rejects, with a 401 that looks like a credential problem.
#
# Asking for the token under the issuer's own Host header closes that gap. It
# mints a token that says exactly who it was minted by.
ISSUER_HOST="${ISSUER_HOST:-keycloak:8080}"

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

log "waiting for $API"
for _ in $(seq 1 60); do
  curl -fsS "$API/health/ready" >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS "$API/health/ready" >/dev/null || fail "the API never became ready"

log "obtaining credentials"
INTERNAL="$(token iron-ledger-internal iron-ledger-internal-secret)"
PROVIDER="$(token provider-a provider-a-secret)"
[ "$INTERNAL" != "null" ] || fail "no internal token"
[ "$PROVIDER" != "null" ] || fail "no provider token"

log "opening a wallet with 100.00 BRL"
# A run id keeps repeated invocations from colliding on the idempotency keys
# below. Reusing a key with a different payload is a 409 by design, so without
# this the script would fail the second time it was run.
RUN="$(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')"
PLAYER="$(cat /proc/sys/kernel/random/uuid)"
WALLET="$(curl -fsS -X POST "$API/wallets" \
  -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' \
  -d "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"100.00\",\"currency\":\"BRL\"}}" \
  | jq -r .id)"
[ "$WALLET" != "null" ] || fail "the wallet was not created"
ok "wallet $WALLET"

# Two distinct bets of 80.00, launched together.
bet() {
  local external="$1"
  curl -sS -o "/tmp/iron-race-$external.json" -w '%{http_code}' \
    -X POST "$API/wagering/transactions" \
    -H "Authorization: Bearer $PROVIDER" -H 'Content-Type: application/json' \
    -H "Idempotency-Key: provider-a:$external" \
    -d "{
          \"providerId\":\"provider-a\",
          \"externalTransactionId\":\"$external\",
          \"playerId\":\"$PLAYER\",
          \"walletId\":\"$WALLET\",
          \"roundId\":\"race-1\",
          \"gameId\":\"fortune-chimp\",
          \"kind\":\"BET\",
          \"money\":{\"amount\":\"80.00\",\"currency\":\"BRL\"}
        }" > "/tmp/iron-race-$external.code"
}

log "submitting two 80.00 bets at the same instant"
bet "race-a-$RUN" & bet "race-b-$RUN" &
wait

processed=0
rejected=0
for external in "race-a-$RUN" "race-b-$RUN"; do
  status="$(cat "/tmp/iron-race-$external.code")"
  body="$(cat "/tmp/iron-race-$external.json")"
  state="$(jq -r '.status // "NONE"' <<<"$body")"
  code="$(jq -r '.failureCode // "-"' <<<"$body")"
  printf '  %s → HTTP %s, %s %s\n' "$external" "$status" "$state" "$code"
  case "$state" in
    PROCESSED) processed=$((processed + 1)) ;;
    REJECTED)  [[ "$code" == "INSUFFICIENT_BALANCE" ]] || fail "$external was rejected with $code, not an overdraft"
              rejected=$((rejected + 1)) ;;
    *) fail "$external ended as $state" ;;
  esac
done

log "asserting the outcome"
[[ "$processed" -eq 1 ]] || fail "$processed bets were processed, want exactly 1"
ok "exactly one bet was processed"
[[ "$rejected" -eq 1 ]] || fail "$rejected bets were refused, want exactly 1"
ok "the other was refused for insufficient funds"

balance="$(curl -fsS -H "Authorization: Bearer $INTERNAL" "$API/wallets/$WALLET" | jq -r .balance.amount)"
[[ "$balance" == "20.00" ]] || fail "the final balance is $balance, want 20.00"
ok "the final balance is 20.00"

debits="$(curl -fsS -H "Authorization: Bearer $INTERNAL" "$API/wallets/$WALLET/ledger?limit=200" \
  | jq '[.data[] | select(.direction == "DEBIT")] | length')"
[[ "$debits" -eq 1 ]] || fail "the ledger holds $debits debits, want exactly 1"
ok "the ledger holds exactly one debit"

log "repeating both bets; a resend must move nothing"
bet "race-a-$RUN" & bet "race-b-$RUN" &
wait
for external in "race-a-$RUN" "race-b-$RUN"; do
  replay="$(jq -r '.idempotentReplay' "/tmp/iron-race-$external.json")"
  [[ "$replay" == "true" ]] || fail "the resend of $external was applied again, not recognised as a replay"
done
ok "both resends were recognised as replays"
balance="$(curl -fsS -H "Authorization: Bearer $INTERNAL" "$API/wallets/$WALLET" | jq -r .balance.amount)"
[[ "$balance" == "20.00" ]] || fail "a resend moved the balance to $balance"
debits="$(curl -fsS -H "Authorization: Bearer $INTERNAL" "$API/wallets/$WALLET/ledger?limit=200" \
  | jq '[.data[] | select(.direction == "DEBIT")] | length')"
[[ "$debits" -eq 1 ]] || fail "a resend produced $debits debits"
ok "resends changed nothing"

log "reconciling"
report="$(curl -fsS -X POST -H "Authorization: Bearer $INTERNAL" "$API/wallets/$WALLET/reconciliation")"
consistent="$(jq -r .consistent <<<"$report")"
difference="$(jq -r .difference.amount <<<"$report")"
[[ "$consistent" == "true" ]] || fail "the wallet diverged by $difference"
ok "the stored balance equals the sum of the ledger"

printf '\n\033[32mAll guarantees held.\033[0m\n'
