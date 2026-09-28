// Package keycloak validates Keycloak access tokens locally against the realm
// JWKS. There is no local identity store and no login flow: this service is an
// OAuth 2.0 resource server and nothing else.
package keycloak

import (
	"fmt"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Config carries the realm parameters. The realm is also the tenant: a token
// from another realm is a token for a different tenant, which is why the tenant
// is derived from the verified issuer rather than from a claim we trust freely.
type Config struct {
	// RealmURL is the canonical base URL of the Keycloak realm, e.g.
	// http://keycloak:8080/realms/ironledger. It is the value the token's `iss`
	// claim is compared against, so it must be the URL the realm issues in its
	// tokens, not necessarily the one this process dials.
	RealmURL string
	// JWKSURL is where the key set is fetched from. It defaults to
	// RealmURL + /protocol/openid-connect/certs, and is set separately when the
	// realm is reachable internally under a different name than it issues in
	// tokens.
	JWKSURL string
	// Audience must appear in the token's `aud`. Configuring the audience mapper
	// on this API's client is a deployment prerequisite: without it every token
	// is rejected.
	Audience string
	// AllowedClients is the accepted `azp` set.
	AllowedClients []string
	// Leeway absorbs clock skew. It is a fixed 30 seconds and not configurable,
	// because a large leeway silently widens the window in which an expired
	// token is still accepted.
	Leeway time.Duration
	// JWKSCacheTTL bounds how long a JWKS document is reused.
	JWKSCacheTTL time.Duration
	// MinRefreshInterval rate-limits refreshes triggered by an unknown `kid`, so
	// a stream of tokens bearing bogus key ids cannot turn into a stream of
	// outbound requests to Keycloak (research.md D-10).
	MinRefreshInterval time.Duration
	// HTTPTimeout bounds a single JWKS fetch.
	HTTPTimeout time.Duration
}

// DefaultConfig returns the production defaults. The JWKS is cached for an hour
// and an unknown kid may force at most one refresh per minute.
func DefaultConfig(realmURL, audience string, allowedClients []string) Config {
	return Config{
		RealmURL:           realmURL,
		Audience:           audience,
		AllowedClients:     allowedClients,
		Leeway:             30 * time.Second,
		JWKSCacheTTL:       time.Hour,
		MinRefreshInterval: time.Minute,
		HTTPTimeout:        5 * time.Second,
	}
}

func (c Config) validate() error {
	if c.RealmURL == "" {
		return fmt.Errorf("keycloak: realm URL is required")
	}
	if c.Audience == "" {
		return fmt.Errorf("keycloak: audience is required; a token without this audience must be rejected")
	}
	if len(c.AllowedClients) == 0 {
		return fmt.Errorf("keycloak: at least one allowed client is required")
	}
	return nil
}

// jwk is one JSON Web Key. Only the fields needed to verify an RS256 signature
// are decoded; an unknown field in the document is ignored rather than rejected,
// because Keycloak may add metadata at any time.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

// jwksCache holds the parsed key set and the time it was fetched.
type jwksCache struct {
	mu        sync.Mutex
	keys      map[string]jwk
	fetchedAt time.Time
	// lastAttempt rate-limits a refresh driven by an unknown kid.
	lastAttempt time.Time
	now         func() time.Time
}

func (c *jwksCache) snapshot() (map[string]jwk, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.keys, c.fetchedAt
}

// stale reports whether the cache must be refetched for a normal request.
func (c *jwksCache) stale(ttl time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now().Sub(c.fetchedAt) >= ttl
}

// mayRefresh reports whether an unknown-kid refresh is allowed right now. It
// takes the lock only to read and update the timestamp, never to hold it across
// a network call.
func (c *jwksCache) mayRefresh(minInterval time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.lastAttempt.IsZero() && c.now().Sub(c.lastAttempt) < minInterval {
		return false
	}
	c.lastAttempt = c.now()
	return true
}

func (c *jwksCache) store(keys map[string]jwk) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys = keys
	c.fetchedAt = c.now()
}

// claims are the token fields this service reads. They are decoded explicitly
// rather than into a map, so an unexpected token shape is a decode error
// instead of a silently absent field. The embedded jwt.RegisteredClaims is what
// the library validates against; azp, scope and tenant_id are ours.
type claims struct {
	// RegisteredClaims supplies iss, sub, aud, exp, nbf and iat. They are NOT
	// redeclared here: a shadowing field would leave the embedded struct empty
	// and the library would validate an all-zero claim set, rejecting every
	// token for looking like it carried nothing.
	jwt.RegisteredClaims

	AuthorizedParty string `json:"azp"`
	Scope           string `json:"scope"`
	// TenantID is used only when the deployment does not derive one realm per
	// tenant. It is optional on purpose: the common topology is one realm.
	TenantID string `json:"tenant_id"`
}

var errNoKeys = fmt.Errorf("keycloak: JWKS contains no usable RS256 key")
