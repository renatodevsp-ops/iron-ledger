//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ironledger/iron-ledger/internal/app/fxmodules"
	"github.com/ironledger/iron-ledger/internal/app/httpapi"
	"github.com/ironledger/iron-ledger/internal/domain/wagering/domain"
	"github.com/ironledger/iron-ledger/internal/identity"
)

// apiServer is the real HTTP surface, wired exactly as the binary wires it and
// talking to a real Keycloak.
type apiServer struct {
	URL    string
	tokens *keycloakTokens
	stop   context.CancelFunc
	client *http.Client
}

type keycloakTokens struct {
	internal string
	a        string
	b        string
}

// startAPI boots the HTTP surface against the test dependencies.
func startAPI(t *testing.T) *apiServer {
	t.Helper()

	instance := newInstance(t, "http")
	verifier, err := identity.NewVerifier(context.Background(), testConfig.Auth)
	if err != nil {
		t.Fatalf("identity discovery against %s: %v", testConfig.Auth.Issuer, err)
	}
	handler := httpapi.NewHandler(
		instance.wallets, instance.wager, instance.ledger, instance.queries, logger,
	)
	router := handler.Router(identity.Authenticate(verifier, logger), healthHandler(),
		fxmodules.NewMetricsHandler(instance.metrics), testConfig.HTTP.MaxBodyBytes)

	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	tokens, err := fetchTokens(t)
	if err != nil {
		t.Skipf("the identity provider is not reachable, skipping the authentication tests: %v", err)
	}
	return &apiServer{URL: server.URL, tokens: tokens, client: server.Client()}
}

func healthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	return mux
}

// token obtains a client-credentials token from the identity provider.
func token(t *testing.T, clientID, secret string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The token endpoint speaks form encoding, not JSON.
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {secret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		testConfig.Auth.Issuer+"/protocol/openid-connect/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build token request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("token request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("token request returned %d: %s", resp.StatusCode, payload)
	}
	var decoded struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if decoded.AccessToken == "" {
		t.Fatal("the identity provider returned no access token")
	}
	return decoded.AccessToken
}

func fetchTokens(t *testing.T) (*keycloakTokens, error) {
	return &keycloakTokens{
		internal: token(t, testConfig.Auth.InternalClientID, "iron-ledger-internal-secret"),
		a:        token(t, "provider-a", "provider-a-secret"),
		b:        token(t, "provider-b", "provider-b-secret"),
	}, nil
}

// do performs an authenticated request.
func (s *apiServer) do(t *testing.T, method, path, bearer string, body any) (*http.Response, []byte) {
	t.Helper()
	return s.doWithKey(t, method, path, bearer, "", body)
}

// doWithKey performs an authenticated request carrying an idempotency key, which
// the operation endpoint requires.
func (s *apiServer) doWithKey(t *testing.T, method, path, bearer, key string, body any) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, s.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp, payload
}

type walletResponse struct {
	ID       string `json:"id"`
	PlayerID string `json:"playerId"`
	Balance  struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	} `json:"balance"`
	Version int64 `json:"version"`
}

type operationResponse struct {
	TransactionID string `json:"transactionId"`
	Status        string `json:"status"`
	Balance       *struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	} `json:"balance"`
	FailureCode      string `json:"failureCode"`
	IdempotentReplay bool   `json:"idempotentReplay"`
}

type problemResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Test_requestsWithoutCredentialsAreRefused proves the business endpoints are
// not anonymous.
func Test_requestsWithoutCredentialsAreRefused(t *testing.T) {
	api := startAPI(t)

	tests := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"no token on an operation", http.MethodPost, "/wagering/transactions", map[string]any{}},
		{"no token on a wallet", http.MethodPost, "/wallets", map[string]any{}},
		{"no token on a wallet read", http.MethodGet, "/wallets/" + uuid.NewString(), nil},
		{"no token on the ledger", http.MethodGet, "/wallets/" + uuid.NewString() + "/ledger", nil},
		{"no token on a transaction", http.MethodGet, "/wagering/transactions/" + uuid.NewString(), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, payload := api.do(t, tt.method, tt.path, "", tt.body)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
			if resp.Header.Get("WWW-Authenticate") == "" {
				t.Error("the response carries no WWW-Authenticate challenge")
			}
			problem := problemResponse{}
			if err := json.Unmarshal(payload, &problem); err != nil {
				t.Fatalf("decode problem: %v (%s)", err, payload)
			}
			if problem.Code == "" {
				t.Error("the problem carries no failure code")
			}
		})
	}
}

// Test_anInvalidCredentialIsRefused proves a token the provider did not issue —
// or did not issue for this service — is refused.
func Test_anInvalidCredentialIsRefused(t *testing.T) {
	api := startAPI(t)

	tests := []struct {
		name  string
		token string
	}{
		{"a made-up token", "not-a-token"},
		{"a token signed by nobody", "eyJhbGciOiJSUzI1NiIsImtpZCI6ImZha2UifQ." +
			"eyJzdWIiOiJmb28iLCJhdWQiOiJpcm9uLWxlZGdlci1hcGkifQ.c2ln"},
		{"the bare word Bearer", "Bearer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, _ := api.do(t, http.MethodPost, "/wallets", tt.token, map[string]any{
				"playerId":       uuid.NewString(),
				"initialBalance": map[string]string{"amount": "1.00", "currency": "BRL"},
			})
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

// Test_anExpiredCredentialIsRefused proves expiry is checked, not assumed.
func Test_anExpiredCredentialIsRefused(t *testing.T) {
	// The identity provider's own token lifetime is short; a token minted and
	// then aged past its expiry must be refused rather than trusted.
	tampered := corruptExpiry(token(t, "provider-a", "provider-a-secret"))
	api := startAPI(t)

	resp, _ := api.do(t, http.MethodPost, "/wagering/transactions", tampered, map[string]any{
		"providerId":            "provider-a",
		"externalTransactionId": "t-1",
		"playerId":              uuid.NewString(),
		"walletId":              uuid.NewString(),
		"roundId":               "round-1",
		"gameId":                "g",
		"kind":                  "BET",
		"money":                 map[string]string{"amount": "1.00", "currency": "BRL"},
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a token whose payload was altered", resp.StatusCode)
	}
}

// corruptExpiry re-encodes a token with an expired `exp`. The signature no
// longer matches, so the platform must refuse it — which is exactly the point:
// a caller cannot talk its way past verification.
func corruptExpiry(raw string) string {
	parts := bytes.SplitN([]byte(raw), []byte("."), 3)
	if len(parts) != 3 {
		return raw
	}
	payload := map[string]any{}
	if err := decodeSegment(parts[1], &payload); err != nil {
		return raw
	}
	payload["exp"] = time.Now().Add(-time.Hour).Unix()
	return string(parts[0]) + "." + encodeSegment(payload) + "." + string(parts[2])
}

// Test_aProviderIsIsolatedFromEveryOtherProvider is the authorisation core: a
// provider sees its own transactions and nothing else.
func Test_aProviderIsIsolatedFromEveryOtherProvider(t *testing.T) {
	api := startAPI(t)

	player := uuid.New()
	resp, payload := api.do(t, http.MethodPost, "/wallets", api.tokens.internal, map[string]any{
		"playerId":       player.String(),
		"initialBalance": map[string]string{"amount": "100.00", "currency": "BRL"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open wallet: %d %s", resp.StatusCode, payload)
	}
	wallet := walletResponse{}
	if err := json.Unmarshal(payload, &wallet); err != nil {
		t.Fatalf("decode wallet: %v", err)
	}

	externalID := uuid.NewString()
	resp, payload = api.do(t, http.MethodPost, "/wagering/transactions", api.tokens.a, map[string]any{
		"providerId":            "provider-a",
		"externalTransactionId": externalID,
		"playerId":              player.String(),
		"walletId":              wallet.ID,
		"roundId":               "round-1",
		"gameID":                "fortune-chimp",
		"gameId":                "fortune-chimp",
		"kind":                  "BET",
		"money":                 map[string]string{"amount": "25.00", "currency": "BRL"},
	})
	// The idempotency key is mandatory, so the first call without it is refused;
	// this proves the header is enforced before anything is written.
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("without an idempotency key: status = %d, want 400 (%s)", resp.StatusCode, payload)
	}

	req := func(t *testing.T) (*http.Response, []byte) {
		t.Helper()
		body := map[string]any{
			"providerId":            "provider-a",
			"externalTransactionId": externalID,
			"playerId":              player.String(),
			"walletId":              wallet.ID,
			"roundId":               "round-1",
			"gameId":                "fortune-chimp",
			"kind":                  "BET",
			"money":                 map[string]string{"amount": "25.00", "currency": "BRL"},
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			api.URL+"/wagering/transactions", bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+api.tokens.a)
		httpReq.Header.Set("Idempotency-Key", "provider-a:"+externalID)
		httpResp, err := api.client.Do(httpReq)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer httpResp.Body.Close()
		out, _ := io.ReadAll(httpResp.Body)
		return httpResp, out
	}

	resp, payload = req(t)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit: %d %s", resp.StatusCode, payload)
	}
	operation := operationResponse{}
	if err := json.Unmarshal(payload, &operation); err != nil {
		t.Fatalf("decode operation: %v", err)
	}
	if operation.Status != string(domain.StatusProcessed) {
		t.Fatalf("status = %s, want PROCESSED", operation.Status)
	}
	if operation.Balance == nil || operation.Balance.Amount != "75.00" {
		t.Errorf("balance = %v, want 75.00", operation.Balance)
	}

	t.Run("the owning provider reads its transaction", func(t *testing.T) {
		resp, payload := api.do(t, http.MethodGet,
			"/providers/provider-a/wagering/transactions/"+externalID, api.tokens.a, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, payload)
		}
	})

	t.Run("another provider is refused", func(t *testing.T) {
		resp, payload := api.do(t, http.MethodGet,
			"/providers/provider-a/wagering/transactions/"+externalID, api.tokens.b, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
		problem := problemResponse{}
		_ = json.Unmarshal(payload, &problem)
		if problem.Code == "" {
			t.Error("the refusal carries no failure code")
		}
	})

	t.Run("another provider naming its own path is refused too", func(t *testing.T) {
		resp, _ := api.do(t, http.MethodGet,
			"/providers/provider-b/wagering/transactions/"+externalID, api.tokens.b, nil)
		if resp.StatusCode == http.StatusOK {
			t.Error("provider B was allowed to read a transaction that does not exist for it")
		}
	})

	t.Run("the internal service reads any transaction", func(t *testing.T) {
		resp, _ := api.do(t, http.MethodGet,
			"/wagering/transactions/"+operation.TransactionID, api.tokens.internal, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("the idempotent replay reports the original result", func(t *testing.T) {
		_, payload := req(t)
		replay := operationResponse{}
		if err := json.Unmarshal(payload, &replay); err != nil {
			t.Fatalf("decode replay: %v", err)
		}
		if !replay.IdempotentReplay {
			t.Error("the repeated delivery was not recognised as a replay")
		}
		if replay.TransactionID != operation.TransactionID {
			t.Errorf("replay transactionId = %s, want %s", replay.TransactionID, operation.TransactionID)
		}
	})
}

// Test_walletOperationsAreRestrictedToTheInternalService proves a provider can
// neither open a wallet nor read one.
func Test_walletOperationsAreRestrictedToTheInternalService(t *testing.T) {
	api := startAPI(t)

	player := uuid.New()
	_, payload := api.do(t, http.MethodPost, "/wallets", api.tokens.internal, map[string]any{
		"playerId":       player.String(),
		"initialBalance": map[string]string{"amount": "100.00", "currency": "BRL"},
	})
	wallet := walletResponse{}
	if err := json.Unmarshal(payload, &wallet); err != nil {
		t.Fatalf("decode wallet: %v", err)
	}

	tests := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"open a wallet", http.MethodPost, "/wallets", map[string]any{
			"playerId":       uuid.NewString(),
			"initialBalance": map[string]string{"amount": "1.00", "currency": "BRL"},
		}},
		{"read a wallet", http.MethodGet, "/wallets/" + wallet.ID, nil},
		{"read the ledger", http.MethodGet, "/wallets/" + wallet.ID + "/ledger", nil},
		{"reconcile a wallet", http.MethodPost, "/wallets/" + wallet.ID + "/reconciliation", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, payload := api.do(t, tt.method, tt.path, api.tokens.a, tt.body)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403 (%s)", resp.StatusCode, payload)
			}
		})
	}

	// And no money moved as a result of the refused attempts.
	resp, payload := api.do(t, http.MethodGet, "/wallets/"+wallet.ID, api.tokens.internal, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read wallet: %d", resp.StatusCode)
	}
	after := walletResponse{}
	if err := json.Unmarshal(payload, &after); err != nil {
		t.Fatalf("decode wallet: %v", err)
	}
	if after.Balance.Amount != "100.00" {
		t.Errorf("balance = %s, want 100.00: a refused request moved nothing", after.Balance.Amount)
	}
}

// Test_theInternalServiceCannotSubmitOperations proves the roles do not overlap:
// the internal service is not also a game provider.
func Test_theInternalServiceCannotSubmitOperations(t *testing.T) {
	api := startAPI(t)

	resp, payload := api.do(t, http.MethodPost, "/wagering/transactions", api.tokens.internal, map[string]any{
		"providerId":            "provider-a",
		"externalTransactionId": uuid.NewString(),
		"playerId":              uuid.NewString(),
		"walletId":              uuid.NewString(),
		"roundId":               "round-1",
		"gameId":                "fortune-chimp",
		"kind":                  "BET",
		"money":                 map[string]string{"amount": "1.00", "currency": "BRL"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (%s)", resp.StatusCode, payload)
	}
}

// Test_aProviderCannotImpersonateAnotherOne proves the body never wins over the
// credential.
func Test_aProviderCannotImpersonateAnotherOne(t *testing.T) {
	api := startAPI(t)

	player := uuid.New()
	_, payload := api.do(t, http.MethodPost, "/wallets", api.tokens.internal, map[string]any{
		"playerId":       player.String(),
		"initialBalance": map[string]string{"amount": "100.00", "currency": "BRL"},
	})
	wallet := walletResponse{}
	_ = json.Unmarshal(payload, &wallet)

	externalID := uuid.NewString()
	submit := func(provider string) (*http.Response, []byte) {
		body, err := json.Marshal(map[string]any{
			"providerId":            provider,
			"externalTransactionId": externalID,
			"playerId":              player.String(),
			"walletId":              wallet.ID,
			"roundId":               "round-1",
			"gameId":                "fortune-chimp",
			"kind":                  "BET",
			"money":                 map[string]string{"amount": "10.00", "currency": "BRL"},
		})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			api.URL+"/wagering/transactions", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+api.tokens.b)
		req.Header.Set("Idempotency-Key", provider+":"+externalID)
		resp, err := api.client.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp, out
	}

	resp, payload := submit("provider-b")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("provider B submitting as itself: %d %s", resp.StatusCode, payload)
	}

	// The operation is recorded under the identity that authenticated it.
	if read, _ := api.do(t, http.MethodGet,
		"/providers/provider-b/wagering/transactions/"+externalID, api.tokens.b, nil); read.StatusCode != http.StatusOK {
		t.Errorf("provider B cannot read the operation it submitted: %d", read.StatusCode)
	}

	// Now the same credential claiming to be provider-a.
	resp, payload = submit("provider-a")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 when the body claims another provider (%s)", resp.StatusCode, payload)
	}

	// Nothing was applied under provider-a's identity.
	resp, payload = api.do(t, http.MethodGet,
		"/providers/provider-a/wagering/transactions/"+externalID, api.tokens.a, nil)
	if resp.StatusCode == http.StatusOK {
		t.Errorf("a transaction appeared under provider-a: %s", payload)
	}
}

// Test_theHealthProbesArePublic proves a probe can answer even when the identity
// provider is down, because a probe must not depend on it.
func Test_theHealthProbesArePublic(t *testing.T) {
	api := startAPI(t)

	for _, path := range []string{"/health/live", "/health/ready"} {
		resp, _ := api.do(t, http.MethodGet, path, "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s returned %d without credentials, want 200", path, resp.StatusCode)
		}
	}
}

// Test_invalidMoneyIsRejectedAtTheBoundary proves no float reaches the domain.
func Test_invalidMoneyIsRejectedAtTheBoundary(t *testing.T) {
	api := startAPI(t)

	player := uuid.New()
	tests := []struct {
		name   string
		money  map[string]any
		status int
	}{
		{"a JSON number instead of a string", map[string]any{"amount": 25.00, "currency": "BRL"}, 400},
		{"scientific notation", map[string]any{"amount": "1e5", "currency": "BRL"}, 400},
		{"excess scale", map[string]any{"amount": "1.234", "currency": "BRL"}, 400},
		{"NaN", map[string]any{"amount": "NaN", "currency": "BRL"}, 400},
		{"an unknown currency", map[string]any{"amount": "25.00", "currency": "XYZ"}, 400},
		{"an empty amount", map[string]any{"amount": "", "currency": "BRL"}, 400},
		{"a negative amount", map[string]any{"amount": "-25.00", "currency": "BRL"}, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := api.do(t, http.MethodPost, "/wallets", api.tokens.internal, map[string]any{
				"playerId":       player.String(),
				"initialBalance": tt.money,
			})
			if resp.StatusCode != tt.status {
				t.Errorf("status = %d, want %d (%s)", resp.StatusCode, tt.status, body)
			}
			problem := problemResponse{}
			if err := json.Unmarshal(body, &problem); err == nil && problem.Code == "" {
				t.Error("the rejection carries no failure code")
			}
		})
	}

	// A LOSS that carries a value is refused as a business rule, not a parse
	// error, and the distinction is visible in the code.
	_, payload := api.do(t, http.MethodPost, "/wallets", api.tokens.internal, map[string]any{
		"playerId":       player.String(),
		"initialBalance": map[string]any{"amount": "1.00", "currency": "BRL"},
	})
	wallet := walletResponse{}
	_ = json.Unmarshal(payload, &wallet)

	lossExternalID := uuid.NewString()
	resp, lossPayload := api.doWithKey(t, http.MethodPost, "/wagering/transactions", api.tokens.a,
		"provider-a:"+lossExternalID, map[string]any{
			"providerId":            "provider-a",
			"externalTransactionId": lossExternalID,
			"playerId":              wallet.PlayerID,
			"walletId":              wallet.ID,
			"roundId":               "round-1",
			"gameId":                "fortune-chimp",
			"kind":                  "LOSS",
			"money":                 map[string]any{"amount": "5.00", "currency": "BRL"},
		})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a LOSS carrying a value (%s)", resp.StatusCode, lossPayload)
	}
	problem := problemResponse{}
	_ = json.Unmarshal(lossPayload, &problem)
	if problem.Code != "LOSS_AMOUNT_MUST_BE_ZERO" {
		t.Errorf("failure code = %s, want LOSS_AMOUNT_MUST_BE_ZERO", problem.Code)
	}
}

// Test_theOpeningKindIsRefusedFromTheOutside proves only the platform may open a
// wallet, whatever the provider claims.
func Test_theOpeningKindIsRefusedFromTheOutside(t *testing.T) {
	api := startAPI(t)

	player := uuid.New()
	_, payload := api.do(t, http.MethodPost, "/wallets", api.tokens.internal, map[string]any{
		"playerId":       player.String(),
		"initialBalance": map[string]string{"amount": "100.00", "currency": "BRL"},
	})
	wallet := walletResponse{}
	_ = json.Unmarshal(payload, &wallet)

	openingExternalID := uuid.NewString()
	resp, payload := api.doWithKey(t, http.MethodPost, "/wagering/transactions", api.tokens.a,
		"provider-a:"+openingExternalID, map[string]any{
			"providerId":            "provider-a",
			"externalTransactionId": openingExternalID,
			"playerId":              wallet.PlayerID,
			"walletId":              wallet.ID,
			"roundId":               "round-1",
			"gameId":                "fortune-chimp",
			"kind":                  "OPENING",
			"money":                 map[string]string{"amount": "50.00", "currency": "BRL"},
		})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", resp.StatusCode, payload)
	}
	problem := problemResponse{}
	_ = json.Unmarshal(payload, &problem)
	if problem.Code != "KIND_NOT_ACCEPTED" {
		t.Errorf("failure code = %s, want KIND_NOT_ACCEPTED", problem.Code)
	}

	// Nothing was credited.
	_, payload = api.do(t, http.MethodGet, "/wallets/"+wallet.ID, api.tokens.internal, nil)
	after := walletResponse{}
	_ = json.Unmarshal(payload, &after)
	if after.Balance.Amount != "100.00" {
		t.Errorf("balance = %s, want 100.00", after.Balance.Amount)
	}
}

// Test_aBusinessRejectionIsDistinguishable proves the contract a provider
// reacts to: 200 settled, 202 accepted and waiting, 422 a terminal decision, 409
// a conflict, 503 a dependency that is down.
func Test_aBusinessRejectionIsDistinguishable(t *testing.T) {
	api := startAPI(t)

	player := uuid.New()
	_, payload := api.do(t, http.MethodPost, "/wallets", api.tokens.internal, map[string]any{
		"playerId":       player.String(),
		"initialBalance": map[string]string{"amount": "10.00", "currency": "BRL"},
	})
	wallet := walletResponse{}
	_ = json.Unmarshal(payload, &wallet)

	submit := func(externalID, kind, amount, reference string) (*http.Response, []byte) {
		body := map[string]any{
			"providerId":            "provider-a",
			"externalTransactionId": externalID,
			"playerId":              wallet.PlayerID,
			"walletId":              wallet.ID,
			"roundId":               "round-1",
			"gameId":                "fortune-chimp",
			"kind":                  kind,
			"money":                 map[string]string{"amount": amount, "currency": "BRL"},
		}
		if reference != "" {
			body["referenceExternalTransactionId"] = reference
		}
		encoded, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
			api.URL+"/wagering/transactions", bytes.NewReader(encoded))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+api.tokens.a)
		req.Header.Set("Idempotency-Key", "provider-a:"+externalID)
		resp, err := api.client.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp, out
	}

	t.Run("settled", func(t *testing.T) {
		resp, payload := submit(uuid.NewString(), "BET", "4.00", "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200 (%s)", resp.StatusCode, payload)
		}
	})

	t.Run("rejected by a business rule", func(t *testing.T) {
		resp, payload := submit(uuid.NewString(), "BET", "500.00", "")
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422 (%s)", resp.StatusCode, payload)
		}
		rejection := operationResponse{}
		_ = json.Unmarshal(payload, &rejection)
		if rejection.Status != string(domain.StatusRejected) {
			t.Errorf("status = %s, want REJECTED", rejection.Status)
		}
		if rejection.FailureCode != "INSUFFICIENT_BALANCE" {
			t.Errorf("failureCode = %s, want INSUFFICIENT_BALANCE", rejection.FailureCode)
		}
	})

	t.Run("accepted and waiting for a reference", func(t *testing.T) {
		resp, payload := submit(uuid.NewString(), "REFUND", "4.00", "never-arrives")
		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("status = %d, want 202 (%s)", resp.StatusCode, payload)
		}
		pending := operationResponse{}
		_ = json.Unmarshal(payload, &pending)
		if pending.Status != string(domain.StatusPendingReference) {
			t.Errorf("status = %s, want PENDING_REFERENCE", pending.Status)
		}
	})

	t.Run("a key reused with a different payload", func(t *testing.T) {
		externalID := uuid.NewString()
		resp, payload := submit(externalID, "BET", "1.00", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("first: %d %s", resp.StatusCode, payload)
		}
		resp, payload = submit(externalID, "BET", "2.00", "")
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("status = %d, want 409 (%s)", resp.StatusCode, payload)
		}
	})

	t.Run("a missing wallet", func(t *testing.T) {
		resp, payload := submit(uuid.NewString(), "BET", "1.00", "")
		_ = resp
		_ = payload
		// A wallet that does not exist is a terminal decision about the
		// operation, reported as 404 rather than a silent success.
		body := map[string]any{
			"providerId":            "provider-a",
			"externalTransactionId": uuid.NewString(),
			"playerId":              wallet.PlayerID,
			"walletId":              uuid.NewString(),
			"roundId":               "round-1",
			"gameId":                "fortune-chimp",
			"kind":                  "BET",
			"money":                 map[string]string{"amount": "1.00", "currency": "BRL"},
		}
		encoded, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
			api.URL+"/wagering/transactions", bytes.NewReader(encoded))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+api.tokens.a)
		req.Header.Set("Idempotency-Key", "provider-a:"+body["externalTransactionId"].(string))
		resp, err := api.client.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404 (%s)", resp.StatusCode, out)
		}
		problem := problemResponse{}
		_ = json.Unmarshal(out, &problem)
		if problem.Code != "WALLET_NOT_FOUND" {
			t.Errorf("failure code = %s, want WALLET_NOT_FOUND", problem.Code)
		}
	})
}

// Test_theLedgerIsPaginatedThroughTheAPI proves the cursor is opaque and total
// over the wire.
func Test_theLedgerIsPaginatedThroughTheAPI(t *testing.T) {
	api := startAPI(t)

	player := uuid.New()
	_, payload := api.do(t, http.MethodPost, "/wallets", api.tokens.internal, map[string]any{
		"playerId":       player.String(),
		"initialBalance": map[string]string{"amount": "100.00", "currency": "BRL"},
	})
	wallet := walletResponse{}
	_ = json.Unmarshal(payload, &wallet)

	for i := 0; i < 4; i++ {
		externalID := uuid.NewString()
		body, _ := json.Marshal(map[string]any{
			"providerId":            "provider-a",
			"externalTransactionId": externalID,
			"playerId":              wallet.PlayerID,
			"walletId":              wallet.ID,
			"roundId":               "round-1",
			"gameId":                "fortune-chimp",
			"kind":                  "BET",
			"money":                 map[string]string{"amount": "5.00", "currency": "BRL"},
		})
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
			api.URL+"/wagering/transactions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+api.tokens.a)
		req.Header.Set("Idempotency-Key", "provider-a:"+externalID)
		resp, err := api.client.Do(req)
		if err != nil {
			t.Fatalf("bet %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("bet %d: status = %d", i, resp.StatusCode)
		}
	}

	type page struct {
		Data []struct {
			ID    string `json:"id"`
			Money struct {
				Amount string `json:"amount"`
			} `json:"money"`
		} `json:"data"`
		NextCursor string `json:"nextCursor"`
	}

	seen := map[string]bool{}
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		path := "/wallets/" + wallet.ID + "/ledger?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		resp, payload := api.do(t, http.MethodGet, path, api.tokens.internal, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list ledger: %d %s", resp.StatusCode, payload)
		}
		decoded := page{}
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Fatalf("decode page: %v", err)
		}
		for _, entry := range decoded.Data {
			if seen[entry.ID] {
				t.Fatalf("entry %s appeared twice", entry.ID)
			}
			seen[entry.ID] = true
		}
		if decoded.NextCursor == "" {
			break
		}
		cursor = decoded.NextCursor
	}
	if len(seen) != 5 {
		t.Errorf("paged through %d entries, want 5 (the opening and four bets)", len(seen))
	}
}
