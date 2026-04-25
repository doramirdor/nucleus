// This file declares the vault.Backend interface — the contract any
// credential-storage backend must satisfy. The OSS gateway uses *Vault
// (OS keychain); a downstream layer (e.g. an alternative backend or a
// multi-tenant deployment) can plug in a different implementation that
// satisfies the same interface.
//
// Interface methods mirror *Vault's exported method set 1:1. Method
// signatures are deliberately tenant-agnostic — a multi-tenant impl is
// expected to bind the tenant via construction (e.g. a per-request
// wrapper), not by adding a parameter here.
//
// AuthDir / DeleteAuthDir model the per-profile OAuth token directory
// the supervisor passes to the mcp-remote bridge. Alternative impls
// must return an isolated, writable directory per profile and treat it
// as ephemeral filesystem state.

package vault

// Backend is the read/write contract for per-profile credential storage
// plus per-profile OAuth token directories.
type Backend interface {
	// Set stores value under (profileID, credKey). Overwrites on conflict.
	Set(profileID, credKey, value string) error

	// Get retrieves the credential or returns ErrNotFound.
	Get(profileID, credKey string) (string, error)

	// Delete removes the credential. Implementations should treat
	// "already absent" as a no-op (no error).
	Delete(profileID, credKey string) error

	// DeleteProfile removes every credential for a profile across the
	// supplied keys. Returns the first error encountered, if any, but
	// continues attempting subsequent deletions.
	DeleteProfile(profileID string, credKeys []string) error

	// AuthDir returns a per-profile directory the supervisor can pass to
	// upstream OAuth bridges (notably mcp-remote) via
	// MCP_REMOTE_CONFIG_DIR. The directory must exist with mode 0700 by
	// the time this returns, and must be isolated from any other
	// profile's tokens.
	AuthDir(profileID string) (string, error)

	// DeleteAuthDir removes the OAuth directory for a profile. No-op if
	// already gone.
	DeleteAuthDir(profileID string) error
}

// Compile-time check: *Vault satisfies Backend.
var _ Backend = (*Vault)(nil)
