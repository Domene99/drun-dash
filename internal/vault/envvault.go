package vault

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// refScheme is the URI scheme every drun-dash secret_ref uses.
const refScheme = "vault://"

// devBackend is the single namespace [EnvVault] serves. A ref names its
// backend right after the scheme: "vault://<backend>/<name>".
const devBackend = "dev"

// envPrefix is prepended to a ref's uppercased name to form the environment
// variable that holds the credential.
const envPrefix = "DRUNDASH_SECRET_"

// EnvVault implements [Vault].
var _ Vault = EnvVault{}

// EnvVault is the development [Vault] backend. It resolves refs of the form
//
//	vault://dev/<name>
//
// by reading the environment variable
//
//	DRUNDASH_SECRET_<NAME>
//
// where <NAME> is <name> uppercased with hyphens replaced by underscores. For
// example:
//
//	vault://dev/anthropic  -> DRUNDASH_SECRET_ANTHROPIC
//	vault://dev/openai-us   -> DRUNDASH_SECRET_OPENAI_US
//
// It exists so local runs work without a real KMS or secret manager. It holds
// no state, never persists or logs the credential, and is safe for concurrent
// use. Production deployments swap in a cloud backend behind the same [Vault]
// interface (a later batch); nothing else changes.
type EnvVault struct{}

// NewEnvVault returns the development [Vault] backend. See [EnvVault] for the
// ref-to-environment mapping.
func NewEnvVault() EnvVault { return EnvVault{} }

// Resolve implements [Vault]. It parses secretRef against the
// "vault://dev/<name>" grammar and reads the mapped environment variable.
//
//   - A ref that does not match the scheme, names a backend other than "dev",
//     or has an empty/illegal name returns [ErrInvalidRef].
//   - A well-formed ref whose mapped environment variable is unset returns
//     [ErrNotFound].
//
// The returned error never contains the credential value. ctx is accepted for
// interface symmetry with future network-backed vaults; this backend does no
// I/O and does not consult it.
func (EnvVault) Resolve(_ context.Context, secretRef string) (Secret, error) {
	name, err := parseDevRef(secretRef)
	if err != nil {
		return Secret{}, err
	}

	value, ok := os.LookupEnv(envVarFor(name))
	if !ok {
		return Secret{}, fmt.Errorf("%w: %s", ErrNotFound, secretRef)
	}
	return Secret{Value: value}, nil
}

// parseDevRef validates secretRef against "vault://dev/<name>" and returns the
// bare <name>. It is total: every input yields either a name or an error, and
// it never panics.
func parseDevRef(secretRef string) (string, error) {
	rest, ok := strings.CutPrefix(secretRef, refScheme)
	if !ok {
		return "", fmt.Errorf("%w: %q (want %sdev/<name>)", ErrInvalidRef, secretRef, refScheme)
	}

	backend, name, ok := strings.Cut(rest, "/")
	if !ok || backend != devBackend {
		return "", fmt.Errorf("%w: %q (want backend %q)", ErrInvalidRef, secretRef, devBackend)
	}

	if !validName(name) {
		return "", fmt.Errorf("%w: %q (name must be non-empty [A-Za-z0-9_-])", ErrInvalidRef, secretRef)
	}
	return name, nil
}

// validName reports whether name is a non-empty run of characters legal for a
// secret name: ASCII letters, digits, hyphen, and underscore. This keeps the
// ref grammar total and guarantees a clean mapping onto an environment
// variable name.
func validName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// envVarFor maps a validated ref name to its environment variable name:
// uppercased, with hyphens folded to underscores, behind [envPrefix].
func envVarFor(name string) string {
	upper := strings.ToUpper(name)
	upper = strings.ReplaceAll(upper, "-", "_")
	return envPrefix + upper
}
