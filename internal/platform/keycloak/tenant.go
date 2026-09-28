package keycloak

import (
	"strings"

	"github.com/ironledger/ironledger/internal/platform/httpapi"
)

// tenantFor derives the tenant from the verified token.
//
// With one realm per tenant the realm itself is the tenant, which is the
// topology the deployment documents and the one this service assumes. A
// `tenant_id` claim overrides that only when the deployment explicitly
// configured a shared realm, and it is honoured only because the signature was
// already verified: an unverified claim is never read.
func (v *Verifier) tenantFor(c *claims) string {
	if c.TenantID != "" {
		return c.TenantID
	}
	return v.cfg.RealmURL
}

// parseScopes reads the space-delimited `scope` claim. The scope string is also
// sometimes delivered as `scp` holding an array; that shape is not accepted,
// because silently reading an empty scope set would turn a misconfiguration
// into a 403 on every request rather than an obvious failure.
func parseScopes(raw string) map[string]bool {
	out := make(map[string]bool)
	for _, scope := range strings.Fields(raw) {
		out[scope] = true
	}
	return out
}

// Verifier is the TokenVerifier the composition root registers.
var _ httpapi.TokenVerifier = (*Verifier)(nil)
