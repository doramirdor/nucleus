package main

import (
	"fmt"
	"os"

	"github.com/doramirdor/nucleusmcp/internal/registry"
	"github.com/doramirdor/nucleusmcp/internal/vault"
)

// openRegistry opens the profile registry at the default path.
func openRegistry() (*registry.Registry, error) {
	path, err := registry.DefaultPath()
	if err != nil {
		return nil, err
	}
	return registry.Open(path)
}

// newVault returns the keychain-backed vault.
func newVault() *vault.Vault { return vault.New() }

// stderrf prints to stderr with a trailing newline. CLI commands use this
// for user-facing messages (stdout stays clean for `serve` mode).
func stderrf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}
