// This file declares the registry.Store interface — the contract any
// profile-storage backend must satisfy. The OSS gateway uses *Registry
// (SQLite); a downstream layer (e.g. an alternative backend or a
// multi-tenant deployment) can plug in a different implementation that
// satisfies the same interface.
//
// Interface methods mirror *Registry's exported method set 1:1. Method
// signatures are deliberately tenant-agnostic — a multi-tenant impl is
// expected to bind the tenant via construction (e.g. a per-request
// wrapper that filters by user_id), not by adding a parameter here.

package registry

// Store is the read/write contract for profile persistence.
type Store interface {
	// Create inserts a new profile. Returns ErrDuplicate if (connector,
	// name) already exists, ErrInvalid if validation fails.
	Create(p Profile) error

	// Get fetches a profile by ID. Returns ErrNotFound if absent.
	Get(id string) (Profile, error)

	// List returns all profiles sorted by connector then name.
	List() ([]Profile, error)

	// ListByConnector returns profiles for a single connector, sorted by
	// name.
	ListByConnector(connector string) ([]Profile, error)

	// Delete removes a profile by ID. Returns ErrNotFound if absent.
	Delete(id string) error

	// SetDefault marks a profile as the default for its connector,
	// clearing the flag on any other profile of the same connector.
	SetDefault(id string) error

	// GetDefault returns the default profile for a connector, or
	// ErrNotFound if none is marked.
	GetDefault(connector string) (Profile, error)
}

// Compile-time check: *Registry satisfies Store.
var _ Store = (*Registry)(nil)
