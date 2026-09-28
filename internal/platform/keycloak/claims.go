package keycloak

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/ironledger/ironledger/internal/platform/httpapi"
)

// Verifier validates bearer tokens against the realm JWKS.
type Verifier struct {
	cfg    Config
	client *http.Client
	cache  *jwksCache
	// newKeyFunc is overridable so a test can exercise the caching and
	// rate-limiting rules without a Keycloak instance.
	newKeyFunc func(ctx context.Context) (map[string]jwk, error)
}

// New builds a verifier. The realm is contacted lazily on the first request that
// needs a key, so a slow or absent Keycloak delays the first call rather than
// the process start.
func New(cfg Config) (*Verifier, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	timeout := cfg.HTTPTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	v := &Verifier{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
		cache:  &jwksCache{now: time.Now},
	}
	v.newKeyFunc = v.fetchJWKS
	return v, nil
}

// Verify implements httpapi.TokenVerifier.
//
// The order is deliberate: signature first, then the registered claims, then
// authorization. Checking the audience before the signature would let an
// unsigned token steer the error message, and checking `azp` before the
// signature would mean authorizing an unverified claim.
func (v *Verifier) Verify(ctx context.Context, tokenString string) (httpapi.Principal, error) {
	claims := &claims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.cfg.RealmURL),
		jwt.WithAudience(v.cfg.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(v.cfg.Leeway),
		jwt.WithIssuedAt(),
	)

	_, err := parser.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			return nil, fmt.Errorf("token header has no kid")
		}
		key, err := v.keyFor(ctx, kid)
		if err != nil {
			return nil, err
		}
		return key, nil
	})
	if err != nil {
		// The detail is not propagated: it names the expected issuer and can
		// describe the key material. The caller gets a plain 401 and the operator
		// finds the reason in the log, correlated by request id.
		return httpapi.Principal{}, fmt.Errorf("keycloak: token rejected")
	}

	// azp identifies the calling client and becomes the audit actor.
	if !slices.Contains(v.cfg.AllowedClients, claims.AuthorizedParty) {
		return httpapi.Principal{}, fmt.Errorf("keycloak: token rejected")
	}

	tenant := v.tenantFor(claims)
	if tenant == "" {
		return httpapi.Principal{}, fmt.Errorf("keycloak: token rejected")
	}

	return httpapi.Principal{
		TenantID: tenant,
		Subject:  claims.Subject,
		ClientID: claims.AuthorizedParty,
		Scopes:   parseScopes(claims.Scope),
	}, nil
}

// keyFor resolves a kid from the cache, fetching on a miss. A miss triggers at
// most one refresh per MinRefreshInterval so forged kids cannot amplify into
// outbound traffic.
func (v *Verifier) keyFor(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	keys, _ := v.cache.snapshot()
	if key, ok := keys[kid]; ok {
		return parseRSAPublicKey(key)
	}
	// Either the cache is empty, it has expired, or this kid is unknown. All three
	// are handled by one refresh, subject to the rate limit.
	if v.cache.mayRefresh(v.cfg.MinRefreshInterval) {
		fresh, err := v.newKeyFunc(ctx)
		if err != nil {
			return nil, fmt.Errorf("keycloak: fetch jwks: %w", err)
		}
		v.cache.store(fresh)
		if key, ok := fresh[kid]; ok {
			return parseRSAPublicKey(key)
		}
	}
	// A key we already hold is still usable even when a refresh was rate-limited.
	if keys, _ = v.cache.snapshot(); len(keys) > 0 {
		if key, ok := keys[kid]; ok {
			return parseRSAPublicKey(key)
		}
	}
	if v.cache.stale(v.cfg.JWKSCacheTTL) && v.cache.mayRefresh(v.cfg.MinRefreshInterval) {
		fresh, err := v.newKeyFunc(ctx)
		if err != nil {
			return nil, fmt.Errorf("keycloak: fetch jwks: %w", err)
		}
		v.cache.store(fresh)
		if key, ok := fresh[kid]; ok {
			return parseRSAPublicKey(key)
		}
	}
	return nil, fmt.Errorf("keycloak: unknown kid")
}

// fetchJWKS retrieves and indexes the realm key set.
func (v *Verifier) fetchJWKS(ctx context.Context) (map[string]jwk, error) {
	url := v.jwksURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("keycloak: jwks returned %d", resp.StatusCode)
	}
	// The document is small and the body is bounded so a misbehaving endpoint
	// cannot stream indefinitely into memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var set jwkSet
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("keycloak: decode jwks: %w", err)
	}
	out := make(map[string]jwk, len(set.Keys))
	for _, key := range set.Keys {
		// Only RSA signing keys are usable: the parser is restricted to RS256, so
		// indexing an EC key would only hide a configuration error.
		if key.Kty != "RSA" || key.Kid == "" || key.N == "" {
			continue
		}
		out[key.Kid] = key
	}
	if len(out) == 0 {
		return nil, errNoKeys
	}
	return out, nil
}

// jwksURL resolves where to fetch the key set from.
func (v *Verifier) jwksURL() string {
	if v.cfg.JWKSURL != "" {
		return v.cfg.JWKSURL
	}
	return strings.TrimSuffix(v.cfg.RealmURL, "/") + "/protocol/openid-connect/certs"
}

// parseRSAPublicKey rebuilds an RSA public key from the JWK's modulus and
// exponent. Anything malformed is refused rather than defaulted.
func parseRSAPublicKey(key jwk) (*rsa.PublicKey, error) {
	if key.Kty != "RSA" {
		return nil, fmt.Errorf("keycloak: key %q is not RSA", key.Kid)
	}
	if key.Alg != "" && key.Alg != "RS256" {
		return nil, fmt.Errorf("keycloak: key %q declares algorithm %q, only RS256 is accepted", key.Kid, key.Alg)
	}
	nBytes, err := base64.RawURLEncoding.DecodeString(key.N)
	if err != nil {
		return nil, fmt.Errorf("keycloak: key %q has a malformed modulus", key.Kid)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(key.E)
	if err != nil {
		return nil, fmt.Errorf("keycloak: key %q has a malformed exponent", key.Kid)
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	if e < 3 {
		return nil, fmt.Errorf("keycloak: key %q has an unusable exponent", key.Kid)
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
}
