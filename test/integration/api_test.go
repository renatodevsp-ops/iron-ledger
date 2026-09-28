package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ironledger/ironledger/internal/domain"
	"github.com/ironledger/ironledger/internal/platform/httpapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubVerifier stands in for Keycloak so the transport can be exercised end to
// end without a running realm. It is deliberately token-string driven: the
// tests assert that the *header* reaches the verifier and that its verdict
// decides the response, which is the part this package is responsible for.
//
// A real signature check is verified in internal/platform/keycloak.
type stubVerifier struct {
	// principals maps a bearer token to the caller it represents.
	principals map[string]httpapi.Principal
	// seen records every token presented, so a test can prove the Authorization
	// header was actually forwarded.
	seen []string
}

func newStubVerifier() *stubVerifier {
	return &stubVerifier{principals: map[string]httpapi.Principal{
		"writer": {
			TenantID: testTenantID,
			Subject:  "svc-writer",
			ClientID: "betting-platform",
			Scopes:   map[string]bool{"wallet:read": true, "wallet:write": true},
		},
		"reader": {
			TenantID: testTenantID,
			Subject:  "svc-reader",
			ClientID: "betting-platform",
			Scopes:   map[string]bool{"wallet:read": true},
		},
		// A valid token from a different tenant. The wallet exists, but the
		// tenant mismatch must deny access rather than return another tenant's
		// balance.
		"other-tenant": {
			TenantID: "tenant-other",
			Subject:  "svc-other",
			ClientID: "betting-platform",
			Scopes:   map[string]bool{"wallet:read": true, "wallet:write": true},
		},
	}}
}

func (s *stubVerifier) Verify(_ context.Context, token string) (httpapi.Principal, error) {
	s.seen = append(s.seen, token)
	p, ok := s.principals[token]
	if !ok {
		return httpapi.Principal{}, fmt.Errorf("stub: unknown token")
	}
	return p, nil
}

type apiHarness struct {
	router   http.Handler
	verifier *stubVerifier
}

func newAPIHarness(t *testing.T) apiHarness {
	t.Helper()
	verifier := newStubVerifier()
	cfg := httpapi.DefaultConfig()
	srv := httpapi.New(cfg, sharedSvc, verifier, healthCheckAlwaysReady, discardLogger{})
	return apiHarness{router: srv.Handler, verifier: verifier}
}

func healthCheckAlwaysReady(context.Context) error { return nil }

type discardLogger struct{}

func (discardLogger) Info(string, ...any)  {}
func (discardLogger) Error(string, ...any) {}

type response struct {
	Code   int
	Header http.Header
	Body   []byte
}

// errorBody mirrors the contract's error envelope so a status code alone cannot
// make a test pass: the reason code is the part a client branches on.
type errorBody struct {
	Error struct {
		Code           string `json:"code"`
		Message        string `json:"message"`
		Field          string `json:"field"`
		AvailableMinor *int64 `json:"availableMinor"`
	} `json:"error"`
}

// operationResult mirrors the contract's POST response.
type operationResult struct {
	WalletID       string   `json:"walletId"`
	IdempotencyKey string   `json:"idempotencyKey"`
	OperationID    string   `json:"operationId"`
	Type           string   `json:"type"`
	Status         string   `json:"status"`
	BetID          *string  `json:"betId"`
	BalanceMinor   int64    `json:"balanceMinor"`
	Currency       string   `json:"currency"`
	LedgerEntryIDs []string `json:"ledgerEntryIds"`
	OccurredAt     string   `json:"occurredAt"`
}

// walletView mirrors the contract's GET response.
type walletView struct {
	ID           string `json:"id"`
	PlayerID     string `json:"playerId"`
	Currency     string `json:"currency"`
	BalanceMinor int64  `json:"balanceMinor"`
	Status       string `json:"status"`
	Version      int64  `json:"version"`
}

func (a apiHarness) do(t *testing.T, method, path, token, idemKey string, body any) response {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)
	return response{Code: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()}
}

func decodeError(t *testing.T, r response) errorBody {
	t.Helper()
	var e errorBody
	require.NoError(t, json.Unmarshal(r.Body, &e), "body was %s", r.Body)
	return e
}

func decodeResult(t *testing.T, r response) operationResult {
	t.Helper()
	var out operationResult
	require.NoError(t, json.Unmarshal(r.Body, &out), "body was %s", r.Body)
	return out
}

func decodeWallet(t *testing.T, r response) walletView {
	t.Helper()
	var out walletView
	require.NoError(t, json.Unmarshal(r.Body, &out), "body was %s", r.Body)
	return out
}

const betPath = "/v1/wallets/%s/operations"

// TestV1SingleBETDebitsTheWallet is quickstart.md V1 over the real router: a bet
// is debited, the balance drops, and one BET_DEBIT entry is recorded.
func TestV1SingleBETDebitsTheWallet(t *testing.T) {
	h := newAPIHarness(t)
	walletID := newWallet(t, 10000, domain.BRL, domain.WalletActive)

	r := h.do(t, http.MethodPost, fmt.Sprintf(betPath, walletID), "writer", "seed-001", map[string]any{
		"type": "BET", "amountMinor": 3000, "currency": "BRL", "transactionId": "txn-seed-1",
	})
	require.Equal(t, http.StatusOK, r.Code, "body was %s", r.Body)

	body := decodeResult(t, r)
	// This fixture seeds 10000 and bets 3000, so the balance lands on 7000.
	assert.Equal(t, int64(7000), body.BalanceMinor)
	assert.Equal(t, "COMPLETED", body.Status)
	assert.Equal(t, "BRL", body.Currency)
	assert.Equal(t, "BET", body.Type)
	require.Len(t, body.LedgerEntryIDs, 1)
	require.NotNil(t, body.BetID)
	assert.Empty(t, r.Header.Get("X-Idempotent-Replay"),
		"quickstart V2: the first execution must not be labelled a replay")
	assert.Equal(t, "/v1/bets/"+*body.BetID, r.Header.Get("Location"),
		"the contract advertises a Location header for the resulting bet")

	var entryType string
	require.NoError(t, sharedPool.QueryRow(context.Background(),
		`SELECT entry_type FROM ledger_entries WHERE entry_uid = $1`, body.LedgerEntryIDs[0]).Scan(&entryType))
	assert.Equal(t, "BET_DEBIT", entryType)

	assert.Equal(t, int64(7000), balanceOf(t, walletID))
}

// The negative variant of V1: a bet the wallet cannot cover is refused, no
// ledger entry is written, and the refusal is audited (FR-030).
func TestV1InsufficientFundsWritesNoEntryButAudits(t *testing.T) {
	h := newAPIHarness(t)
	walletID := newWallet(t, 3000, domain.BRL, domain.WalletActive)
	before := countRows(t, `SELECT count(*) FROM ledger_entries WHERE wallet_id = $1`, walletID)

	r := h.do(t, http.MethodPost, fmt.Sprintf(betPath, walletID), "writer", "seed-002", map[string]any{
		"type": "BET", "amountMinor": 9000, "currency": "BRL", "transactionId": "txn-seed-2",
	})
	require.Equal(t, http.StatusConflict, r.Code, "body was %s", r.Body)

	e := decodeError(t, r)
	assert.Equal(t, "INSUFFICIENT_FUNDS", e.Error.Code)
	require.NotNil(t, e.Error.AvailableMinor)
	assert.Equal(t, int64(3000), *e.Error.AvailableMinor,
		"availableMinor is the wallet's real balance, so the caller can render a useful message")

	assert.Equal(t, before, countRows(t,
		`SELECT count(*) FROM ledger_entries WHERE wallet_id = $1`, walletID),
		"a refused operation must not append an entry")
	assert.Equal(t, 1, countRows(t,
		`SELECT count(*) FROM audit_log WHERE wallet_id = $1 AND outcome = 'REJECTED'`, walletID),
		"a refused operation must still be audited")
	assert.Equal(t, int64(3000), balanceOf(t, walletID))
}

// TestV2IdempotentReplay is quickstart.md V2: three identical requests, one
// effect, two labelled replays with a byte-identical body.
func TestV2IdempotentReplay(t *testing.T) {
	h := newAPIHarness(t)
	walletID := newWallet(t, 10000, domain.BRL, domain.WalletActive)
	payload := map[string]any{
		"type": "BET", "amountMinor": 1000, "currency": "BRL", "transactionId": "txn-seed-3",
	}

	first := h.do(t, http.MethodPost, fmt.Sprintf(betPath, walletID), "writer", "seed-003", payload)
	require.Equal(t, http.StatusOK, first.Code, "body was %s", first.Body)

	for i := range 2 {
		again := h.do(t, http.MethodPost, fmt.Sprintf(betPath, walletID), "writer", "seed-003", payload)
		require.Equal(t, http.StatusOK, again.Code)
		assert.Equal(t, "true", again.Header.Get("X-Idempotent-Replay"),
			"replay %d must be labelled as such", i+1)
		assert.Equal(t, first.Body, again.Body,
			"a replay must return the stored body byte for byte, not a freshly built one")
	}

	assert.Equal(t, int64(9000), balanceOf(t, walletID), "the wallet was debited exactly once")
	assert.Equal(t, 1, countRows(t,
		`SELECT count(*) FROM ledger_entries WHERE wallet_id = $1`, walletID))
}

// The conflict variant of V2: the same key with a different body is a conflict,
// and the stored result is deliberately not replayed.
func TestV2SameKeyDifferentBodyIsAConflict(t *testing.T) {
	h := newAPIHarness(t)
	walletID := newWallet(t, 10000, domain.BRL, domain.WalletActive)

	first := h.do(t, http.MethodPost, fmt.Sprintf(betPath, walletID), "writer", "seed-003", map[string]any{
		"type": "BET", "amountMinor": 1000, "currency": "BRL", "transactionId": "txn-seed-3",
	})
	require.Equal(t, http.StatusOK, first.Code)

	conflict := h.do(t, http.MethodPost, fmt.Sprintf(betPath, walletID), "writer", "seed-003", map[string]any{
		"type": "BET", "amountMinor": 2000, "currency": "BRL", "transactionId": "txn-seed-3",
	})
	require.Equal(t, http.StatusConflict, conflict.Code, "body was %s", conflict.Body)
	assert.Equal(t, "IDEMPOTENCY_KEY_CONFLICT", decodeError(t, conflict).Error.Code)
	assert.NotEqual(t, first.Body, conflict.Body,
		"replaying the stored result here would be wrong: the request differs")
	assert.Equal(t, int64(9000), balanceOf(t, walletID))
}

func TestAuthIsRequired(t *testing.T) {
	h := newAPIHarness(t)
	walletID := newWallet(t, 10000, domain.BRL, domain.WalletActive)
	path := fmt.Sprintf(betPath, walletID)

	missing := h.do(t, http.MethodPost, path, "", "k1", map[string]any{
		"type": "BET", "amountMinor": 100, "currency": "BRL", "transactionId": "t1",
	})
	assert.Equal(t, http.StatusUnauthorized, missing.Code)
	assert.Equal(t, "UNAUTHORIZED", decodeError(t, missing).Error.Code)

	bad := h.do(t, http.MethodPost, path, "not-a-real-token", "k1", map[string]any{
		"type": "BET", "amountMinor": 100, "currency": "BRL", "transactionId": "t1",
	})
	assert.Equal(t, http.StatusUnauthorized, bad.Code)

	assert.Equal(t, int64(10000), balanceOf(t, walletID), "no rejected request may have any effect")
}

func TestWriteScopeIsRequiredToMutate(t *testing.T) {
	h := newAPIHarness(t)
	walletID := newWallet(t, 10000, domain.BRL, domain.WalletActive)

	// A read-only token must not be able to spend.
	r := h.do(t, http.MethodPost, fmt.Sprintf(betPath, walletID), "reader", "k1", map[string]any{
		"type": "BET", "amountMinor": 100, "currency": "BRL", "transactionId": "t1",
	})
	require.Equal(t, http.StatusForbidden, r.Code, "body was %s", r.Body)
	assert.Equal(t, "FORBIDDEN", decodeError(t, r).Error.Code)
	assert.Equal(t, int64(10000), balanceOf(t, walletID))

	// The same token may still read.
	read := h.do(t, http.MethodGet, "/v1/wallets/"+walletID, "reader", "", nil)
	assert.Equal(t, http.StatusOK, read.Code)
}

// A token from another tenant must not reach a wallet it does not own. This is
// the check that makes the tenant come from the token rather than the path.
func TestTenantIsTakenFromTheToken(t *testing.T) {
	h := newAPIHarness(t)
	walletID := newWallet(t, 10000, domain.BRL, domain.WalletActive)

	// The contract states a cross-tenant read is refused with 403
	// TENANT_MISMATCH, so the status is asserted rather than merely "not 200".
	read := h.do(t, http.MethodGet, "/v1/wallets/"+walletID, "other-tenant", "", nil)
	require.Equal(t, http.StatusForbidden, read.Code, "body was %s", read.Body)
	assert.Equal(t, "TENANT_MISMATCH", decodeError(t, read).Error.Code)

	write := h.do(t, http.MethodPost, fmt.Sprintf(betPath, walletID), "other-tenant", "k1", map[string]any{
		"type": "BET", "amountMinor": 100, "currency": "BRL", "transactionId": "t1",
	})
	require.Equal(t, http.StatusForbidden, write.Code, "body was %s", write.Body)
	assert.Equal(t, "TENANT_MISMATCH", decodeError(t, write).Error.Code)
	assert.Equal(t, int64(10000), balanceOf(t, walletID))
}

func TestGetWalletReturnsTheBalance(t *testing.T) {
	h := newAPIHarness(t)
	walletID := newWallet(t, 10000, domain.BRL, domain.WalletActive)

	r := h.do(t, http.MethodGet, "/v1/wallets/"+walletID, "writer", "", nil)
	require.Equal(t, http.StatusOK, r.Code, "body was %s", r.Body)

	body := decodeWallet(t, r)
	assert.Equal(t, walletID, body.ID)
	assert.Equal(t, "BRL", body.Currency)
	assert.Equal(t, "ACTIVE", body.Status)
	assert.Equal(t, int64(10000), body.BalanceMinor)
	assert.NotEmpty(t, body.PlayerID)
}

func TestRequestValidationErrors(t *testing.T) {
	h := newAPIHarness(t)
	walletID := newWallet(t, 10000, domain.BRL, domain.WalletActive)
	path := fmt.Sprintf(betPath, walletID)

	cases := []struct {
		name   string
		key    string
		body   string
		reason string
	}{
		{"missing idempotency key", "", `{"type":"BET","amountMinor":100,"currency":"BRL","transactionId":"t1"}`, "MISSING_IDEMPOTENCY_KEY"},
		{"unknown field", "k", `{"type":"BET","amountMinor":100,"currency":"BRL","transactionId":"t1","freeBet":true}`, "UNKNOWN_FIELD"},
		{"fractional amount", "k", `{"type":"BET","amountMinor":10.5,"currency":"BRL","transactionId":"t1"}`, "INVALID_AMOUNT"},
		{"exponent notation", "k", `{"type":"BET","amountMinor":1e3,"currency":"BRL","transactionId":"t1"}`, "INVALID_AMOUNT"},
		{"negative amount", "k", `{"type":"BET","amountMinor":-100,"currency":"BRL","transactionId":"t1"}`, "INVALID_AMOUNT"},
		{"unknown operation type", "k", `{"type":"TIP","amountMinor":100,"currency":"BRL","transactionId":"t1"}`, "UNKNOWN_OPERATION_TYPE"},
		{"unknown currency", "k", `{"type":"BET","amountMinor":100,"currency":"XYZ","transactionId":"t1"}`, "INVALID_CURRENCY"},
		{"missing transaction id", "k", `{"type":"BET","amountMinor":100,"currency":"BRL"}`, "MISSING_FIELD"},
		{"body is not an object", "k", `[1,2,3]`, "INVALID_OPERATION_STATE"},
		{"body is not json", "k", `nonsense`, "INVALID_OPERATION_STATE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Authorization", "Bearer writer")
			req.Header.Set("Content-Type", "application/json")
			if tc.key != "" {
				req.Header.Set("Idempotency-Key", tc.key)
			}
			rec := httptest.NewRecorder()
			h.router.ServeHTTP(rec, req)

			require.Equal(t, http.StatusBadRequest, rec.Code, "body was %s", rec.Body.String())
			e := decodeError(t, response{Body: rec.Body.Bytes()})
			assert.Equal(t, tc.reason, e.Error.Code)
		})
	}
}

func TestMalformedWalletIDIsNotFoundNotAServerError(t *testing.T) {
	h := newAPIHarness(t)

	r := h.do(t, http.MethodGet, "/v1/wallets/not-a-uuid", "writer", "", nil)
	assert.Equal(t, http.StatusNotFound, r.Code,
		"a malformed id must be a 404, never a 500 from a failed cast")
}

func TestUnknownRouteIsNotFound(t *testing.T) {
	h := newAPIHarness(t)
	r := h.do(t, http.MethodGet, "/v1/unknown", "writer", "", nil)
	assert.Equal(t, http.StatusNotFound, r.Code)
}

func TestHealthProbesNeedNoCredentials(t *testing.T) {
	h := newAPIHarness(t)

	live := h.do(t, http.MethodGet, "/health/live", "", "", nil)
	assert.Equal(t, http.StatusOK, live.Code)

	ready := h.do(t, http.MethodGet, "/health/ready", "", "", nil)
	assert.Equal(t, http.StatusOK, ready.Code, "body was %s", ready.Body)
}

// Every response carries a request id so a caller can quote it and an operator
// can find the log line.
func TestResponsesCarryARequestID(t *testing.T) {
	h := newAPIHarness(t)
	walletID := newWallet(t, 1000, domain.BRL, domain.WalletActive)

	r := h.do(t, http.MethodPost, fmt.Sprintf(betPath, walletID), "writer", "k1", map[string]any{
		"type": "BET", "amountMinor": 100, "currency": "BRL", "transactionId": "t1",
	})
	require.NotEmpty(t, r.Header.Get("X-Request-Id"))

	// A caller-supplied id is deliberately not adopted: a forgeable correlation
	// id would let one caller splice their request into another's log lines. The
	// server issues its own instead.
	req := httptest.NewRequest(http.MethodGet, "/v1/wallets/"+walletID, nil)
	req.Header.Set("Authorization", "Bearer writer")
	req.Header.Set("X-Request-Id", "trace-abc")
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	assert.NotEqual(t, "trace-abc", rec.Header().Get("X-Request-Id"))
	assert.NotEmpty(t, rec.Header().Get("X-Request-Id"))
}

// A panic anywhere behind the router must become a 500 carrying a request id,
// not a dropped connection that leaves the caller guessing. The readiness probe
// is used as the trigger because a panicking dependency is a realistic failure
// and needs no production hook to provoke it.
func TestPanicBehindTheRouterBecomesAServerError(t *testing.T) {
	srv := httpapi.New(httpapi.DefaultConfig(), sharedSvc, newStubVerifier(),
		func(context.Context) error { panic("database client is corrupt") },
		discardLogger{})

	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("X-Request-Id"),
		"even a recovered panic must be traceable")
}
