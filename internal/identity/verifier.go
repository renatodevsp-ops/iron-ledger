// Package identity authenticates callers against an external OpenID Connect
// provider and authorises what they may do.
//
// The service issues no credentials of its own: no password storage, no token
// minting. It verifies the signature, the issuer, the audience and the expiry
// of a token the IdP issued, and then decides what the caller is allowed to
// touch. Keeping verification and authorisation apart matters, because the
// first is a cryptographic question and the second is a business one.
package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/ironledger/iron-ledger/internal/platform/config"
)

// Errors returned when a credential is missing or unusable.
var (
	// ErrNoCredentials is returned when no bearer token was presented.
	ErrNoCredentials = errors.New("identity: no credentials presented")
	// ErrInvalidToken is returned when the token fails verification.
	ErrInvalidToken = errors.New("identity: invalid token")
	// ErrUnauthorised is returned when a valid caller may not perform the
	// operation.
	ErrUnauthorised = errors.New("identity: not allowed to perform this operation")
)

// Principal is the authenticated caller.
//
// ProviderID is empty for the internal service and set for a game provider,
// which is what scopes every provider-facing query and command.
type Principal struct {
	// Subject is the `sub` claim: the service account's identity.
	Subject string
	// ClientID is the OAuth client the token was issued to.
	ClientID string
	// ProviderID is the game provider the caller may act as, empty for the
	// internal service.
	ProviderID string
	// Roles are the roles the IdP asserted.
	Roles []string
	// Internal reports whether the caller may perform wallet operations.
	Internal bool
}

// IsProvider reports whether the caller acts as a game provider.
func (p Principal) IsProvider() bool { return p.ProviderID != "" && !p.Internal }

// String renders the principal for logs. It carries no credential material.
func (p Principal) String() string {
	if p.Internal {
		return fmt.Sprintf("internal(client=%s)", p.ClientID)
	}
	return fmt.Sprintf("provider(provider=%s, client=%s)", p.ProviderID, p.ClientID)
}

// Verifier validates bearer tokens against the configured IdP.
//
// Keys are fetched lazily and cached by the underlying library, refreshing on
// an unknown key id, so a key rotation in Keycloak does not require a restart.
type Verifier struct {
	provider   *oidc.Provider
	verifier   *oidc.IDTokenVerifier
	settings   config.Auth
	mu         sync.RWMutex
	lastIssuer string
}

// NewVerifier performs OIDC discovery against the configured issuer.
func NewVerifier(ctx context.Context, cfg config.Auth) (*Verifier, error) {
	if !cfg.Enabled {
		return &Verifier{settings: cfg}, nil
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("identity: discover issuer %s: %w", cfg.Issuer, err)
	}
	return &Verifier{
		provider:   provider,
		verifier:   provider.Verifier(&oidc.Config{ClientID: cfg.Audience, Now: time.Now}),
		settings:   cfg,
		lastIssuer: cfg.Issuer,
	}, nil
}

// Enabled reports whether the identity provider is in use.
func (v *Verifier) Enabled() bool { return v != nil && v.settings.Enabled }

// audience reads the `aud` claim, which OIDC allows to be either a single
// string or an array of strings. Identity providers differ, so both spellings
// are accepted rather than making the choice into a deployment accident.
type audience []string

// UnmarshalJSON accepts a string or an array of strings.
func (a *audience) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*a = audience{single}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("identity: aud is neither a string nor an array: %w", err)
	}
	*a = many
	return nil
}

// claims is the subset of the token this service reads.
type claims struct {
	Subject   string   `json:"sub"`
	Audience  audience `json:"aud"`
	Issuer    string   `json:"iss"`
	ExpiresAt int64    `json:"exp"`
	NotBefore int64    `json:"nbf"`
	IssuedAt  int64    `json:"iat"`
	Azp       string   `json:"azp"`
	Roles     []string `json:"roles"`
	RealmRole struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
	ProviderID string `json:"providerId"`
}

// Verify validates a raw token and resolves the principal behind it.
//
// It refuses a token whose issuer is not the configured one, whose audience
// does not include this service, or that is expired — including one that
// expired a second ago. The leeway only covers clock drift between this process
// and the IdP.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (Principal, error) {
	if !v.settings.Enabled {
		// Authentication disabled is a deliberate local-development setting.
		// It grants only the internal role, never a provider identity, so a
		// misconfigured deployment cannot turn into an open provider API.
		return Principal{Subject: "local", ClientID: "local", Internal: true}, nil
	}
	if strings.TrimSpace(rawToken) == "" {
		return Principal{}, ErrNoCredentials
	}

	v.mu.RLock()
	verifier := v.verifier
	v.mu.RUnlock()

	idToken, err := verifier.Verify(ctx, rawToken)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	var parsed claims
	if err := idToken.Claims(&parsed); err != nil {
		return Principal{}, fmt.Errorf("%w: unreadable claims: %v", ErrInvalidToken, err)
	}

	now := time.Now()
	if err := v.checkTimes(parsed, now); err != nil {
		return Principal{}, err
	}

	clientID := parsed.Azp
	if clientID == "" && len(parsed.Audience) > 0 {
		clientID = parsed.Audience[0]
	}

	principal := Principal{
		Subject:    parsed.Subject,
		ClientID:   clientID,
		ProviderID: v.resolveProviderID(parsed, clientID),
		Roles:      append(append([]string{}, parsed.Roles...), parsed.RealmRole.Roles...),
	}

	for _, role := range principal.Roles {
		if role == v.settings.InternalRole {
			principal.Internal = true
		}
	}
	if principal.Internal {
		// An internal caller is never a provider: the internal service may not
		// submit operations on a provider's behalf.
		principal.ProviderID = ""
	}
	if !principal.Internal && principal.ProviderID == "" {
		return Principal{}, fmt.Errorf("%w: client %q is not mapped to a provider and holds no internal role",
			ErrUnauthorised, clientID)
	}
	return principal, nil
}

func (v *Verifier) checkTimes(parsed claims, now time.Time) error {
	leeway := v.settings.Leeway
	if parsed.ExpiresAt != 0 && now.After(time.Unix(parsed.ExpiresAt, 0).Add(leeway)) {
		return fmt.Errorf("%w: token expired at %s", ErrInvalidToken,
			time.Unix(parsed.ExpiresAt, 0).UTC().Format(time.RFC3339))
	}
	if parsed.NotBefore != 0 && now.Add(leeway).Before(time.Unix(parsed.NotBefore, 0)) {
		return fmt.Errorf("%w: token is not valid before %s", ErrInvalidToken,
			time.Unix(parsed.NotBefore, 0).UTC().Format(time.RFC3339))
	}
	return nil
}

// resolveProviderID determines which provider a client may act as.
//
// The token claim wins when the IdP projects one, because it is the IdP's own
// statement about the client. The configured mapping is the fallback for IdPs
// that do not, and it is validated on every request rather than trusted from a
// cache, so removing a provider from the configuration immediately stops it.
func (v *Verifier) resolveProviderID(parsed claims, clientID string) string {
	if parsed.ProviderID != "" {
		return parsed.ProviderID
	}
	return v.settings.ProviderForClient(clientID)
}

// Ready verifies the IdP is reachable, backing the readiness probe.
func (v *Verifier) Ready(ctx context.Context) error {
	if !v.settings.Enabled {
		return nil
	}
	if v.provider == nil {
		return errors.New("identity: provider not initialised")
	}
	if err := v.provider.Claims(ctx); err != nil {
		return fmt.Errorf("identity: claims endpoint: %w", err)
	}
	return nil
}
