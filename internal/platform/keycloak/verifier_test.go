package keycloak

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testIssuer = "http://keycloak.test/realms/ironledger"
	testAud    = "wallet-api"
	testAzp    = "betting-platform"
)

type harness struct {
	verifier *Verifier
	signer   *rsa.PrivateKey
	kid      string
	base     jwt.RegisteredClaims
	scopes   string
	tenant   string
	// fetches counts JWKS requests, so the caching and rate-limit rules can be
	// asserted without counting on timing.
	fetches atomic.Int32
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	h := &harness{signer: key, kid: "kid-1", scopes: "wallet:read wallet:write"}
	h.base = jwt.RegisteredClaims{
		Issuer:    testIssuer,
		Subject:   "service-account-1",
		Audience:  jwt.ClaimStrings{testAud},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-time.Minute)),
	}

	v, err := New(DefaultConfig(testIssuer, testAud, []string{testAzp, "another-client"}))
	require.NoError(t, err)
	h.verifier = v
	return h
}

// installJWKS points the verifier at an in-process realm serving one key, and
// counts the requests.
func (h *harness) installJWKS(t *testing.T) {
	t.Helper()
	doc := jwkSet{Keys: []jwk{{
		Kty: "RSA",
		Kid: h.kid,
		Alg: "RS256",
		Use: "sig",
		N:   base64.RawURLEncoding.EncodeToString(h.signer.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(h.signer.E)).Bytes()),
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h.fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	// The token is issued by testIssuer; the key set is fetched from the test
	// server. Separating the two is exactly what JWKSURL is for.
	h.verifier.cfg.JWKSURL = srv.URL
}

func (h *harness) sign(t *testing.T, mutate func(c jwt.MapClaims)) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":   h.base.Issuer,
		"sub":   h.base.Subject,
		"aud":   []string{testAud},
		"exp":   h.base.ExpiresAt.Unix(),
		"iat":   h.base.IssuedAt.Unix(),
		"azp":   testAzp,
		"sco":   nil,
		"scope": h.scopes,
	}
	delete(claims, "sco")
	if h.tenant != "" {
		claims["tenant_id"] = h.tenant
	}
	if mutate != nil {
		mutate(claims)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = h.kid
	signed, err := tok.SignedString(h.signer)
	require.NoError(t, err)
	return signed
}

func TestVerifyAcceptsAValidToken(t *testing.T) {
	h := newHarness(t)
	h.installJWKS(t)

	p, err := h.verifier.Verify(context.Background(), h.sign(t, nil))
	require.NoError(t, err)
	assert.Equal(t, testAzp, p.ClientID)
	assert.Equal(t, "service-account-1", p.Subject)
	assert.True(t, p.HasScope("wallet:write"))
	assert.True(t, p.HasScope("wallet:read"))
	assert.False(t, p.HasScope("wallet:admin"))
	// With one realm per tenant the realm URL is the tenant.
	assert.Equal(t, testIssuer, p.TenantID)
}

func TestVerifyRejectsAWrongAudience(t *testing.T) {
	h := newHarness(t)
	h.installJWKS(t)

	token := h.sign(t, func(c jwt.MapClaims) { c["aud"] = []string{"some-other-api"} })
	_, err := h.verifier.Verify(context.Background(), token)
	require.Error(t, err)
}

func TestVerifyRejectsAWrongIssuer(t *testing.T) {
	h := newHarness(t)
	h.installJWKS(t)

	token := h.sign(t, func(c jwt.MapClaims) { c["iss"] = "https://evil.test/realms/other" })
	_, err := h.verifier.Verify(context.Background(), token)
	require.Error(t, err)
}

func TestVerifyRejectsAnUnknownClient(t *testing.T) {
	h := newHarness(t)
	h.installJWKS(t)

	token := h.sign(t, func(c jwt.MapClaims) { c["azp"] = "unknown-client" })
	_, err := h.verifier.Verify(context.Background(), token)
	require.Error(t, err, "a token from a client outside the allowlist must not authorize")
}

func TestVerifyRejectsAnExpiredToken(t *testing.T) {
	h := newHarness(t)
	h.installJWKS(t)

	// Well beyond the 30s leeway, so the test cannot pass by accident.
	token := h.sign(t, func(c jwt.MapClaims) {
		c["exp"] = time.Now().Add(-10 * time.Minute).Unix()
		c["iat"] = time.Now().Add(-11 * time.Minute).Unix()
	})
	_, err := h.verifier.Verify(context.Background(), token)
	require.Error(t, err)
}

func TestVerifyRequiresAnExpiry(t *testing.T) {
	h := newHarness(t)
	h.installJWKS(t)

	token := h.sign(t, func(c jwt.MapClaims) { delete(c, "exp") })
	_, err := h.verifier.Verify(context.Background(), token)
	require.Error(t, err, "a token with no exp must be rejected, not treated as never expiring")
}

// A token signed with a different algorithm must be refused even if it carries
// a kid the cache knows: this is the algorithm-confusion attack.
func TestVerifyRejectsANonRS256Algorithm(t *testing.T) {
	h := newHarness(t)
	h.installJWKS(t)

	claims := jwt.MapClaims{
		"iss": testIssuer, "sub": "s", "aud": []string{testAud},
		"exp": time.Now().Add(time.Minute).Unix(), "azp": testAzp, "scope": "wallet:read",
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = h.kid
	signed, err := tok.SignedString([]byte("secret"))
	require.NoError(t, err)

	_, err = h.verifier.Verify(context.Background(), signed)
	require.Error(t, err, "an HS256 token must never be accepted as RS256")
}

func TestVerifyRejectsASignatureFromAnotherKey(t *testing.T) {
	h := newHarness(t)
	h.installJWKS(t)

	other, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": testIssuer, "sub": "s", "aud": []string{testAud},
		"exp": time.Now().Add(time.Minute).Unix(), "azp": testAzp, "scope": "wallet:read",
	})
	tok.Header["kid"] = h.kid
	signed, err := tok.SignedString(other)
	require.NoError(t, err)

	_, err = h.verifier.Verify(context.Background(), signed)
	require.Error(t, err, "a valid-looking token signed by an unknown key must be refused")
}

func TestVerifyCachesTheJWKS(t *testing.T) {
	h := newHarness(t)
	h.installJWKS(t)

	token := h.sign(t, nil)
	for range 5 {
		_, err := h.verifier.Verify(context.Background(), token)
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), h.fetches.Load(),
		"the key set must be fetched once and reused, not once per request")
}

// An unknown kid may force a refresh, but at most once per MinRefreshInterval,
// so forged key ids cannot be turned into outbound traffic to Keycloak.
func TestUnknownKidRefreshIsRateLimited(t *testing.T) {
	h := newHarness(t)
	h.installJWKS(t)
	h.verifier.cfg.MinRefreshInterval = time.Hour

	forged := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": testIssuer, "sub": "s", "aud": []string{testAud},
		"exp": time.Now().Add(time.Minute).Unix(), "azp": testAzp,
	})
	forged.Header["kid"] = "forged-kid"
	signed, err := forged.SignedString(h.signer)
	require.NoError(t, err)

	_, err = h.verifier.Verify(context.Background(), signed)
	require.Error(t, err)
	after := h.fetches.Load()

	for range 20 {
		_, err = h.verifier.Verify(context.Background(), signed)
		require.Error(t, err)
	}
	assert.Equal(t, after, h.fetches.Load(),
		"repeated unknown kids must not each trigger a JWKS fetch")
	assert.LessOrEqual(t, after, int32(1), "the first unknown kid may refresh at most once")
}

func TestVerifyRejectsGarbage(t *testing.T) {
	h := newHarness(t)
	h.installJWKS(t)

	for _, token := range []string{"", "not-a-jwt", "a.b.c", "....."} {
		_, err := h.verifier.Verify(context.Background(), token)
		require.Error(t, err)
	}
}

func TestTenantClaimOverridesTheRealmOnlyForASharedRealm(t *testing.T) {
	h := newHarness(t)
	h.installJWKS(t)
	h.tenant = "tenant-42"

	p, err := h.verifier.Verify(context.Background(), h.sign(t, nil))
	require.NoError(t, err)
	assert.Equal(t, "tenant-42", p.TenantID,
		"an explicit tenant claim is honoured once the signature is verified")
}

func TestConfigValidationRefusesAnEmptyAudience(t *testing.T) {
	// An empty audience would accept any token aimed at any API, which is the
	// deployment mistake quickstart.md calls out. It must fail at startup.
	_, err := New(DefaultConfig(testIssuer, "", []string{testAzp}))
	require.Error(t, err)

	_, err = New(DefaultConfig(testIssuer, testAud, nil))
	require.Error(t, err)

	_, err = New(DefaultConfig("", testAud, []string{testAzp}))
	require.Error(t, err)
}

func TestParseRSAPublicKeyRejectsMalformedKeys(t *testing.T) {
	_, err := parseRSAPublicKey(jwk{Kty: "EC", Kid: "k"})
	require.Error(t, err)

	_, err = parseRSAPublicKey(jwk{Kty: "RSA", Kid: "k", Alg: "RS512",
		N: base64.RawURLEncoding.EncodeToString(big.NewInt(1).Bytes()),
		E: base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})})
	require.Error(t, err, "a key declaring an algorithm other than RS256 must be refused")

	_, err = parseRSAPublicKey(jwk{Kty: "RSA", Kid: "k", N: "!!!not-base64!!!", E: "AQAB"})
	require.Error(t, err)
}

func TestScopesAreParsedAsADelimitedSet(t *testing.T) {
	got := parseScopes("  wallet:read   wallet:write ")
	assert.Equal(t, map[string]bool{"wallet:read": true, "wallet:write": true}, got)
	assert.Empty(t, parseScopes(""))
}
