// Package access defines request identity independently of MCP and HTTP.
// A future OAuth adapter resolves an identity here; it must never turn a shared
// administrator key into unrestricted authority for every OAuth user.
package access

import (
	"context"
	"errors"
	"strings"

	"go.xchunk.org/vela/api"
)

// Credential is an upstream Vela credential, deliberately redacted when printed.
// It is never part of tool arguments, results, or application configuration dumps.
type Credential struct{ token string }

func NewCredential(token string) Credential { return Credential{token: token} }
func (c Credential) Token() string          { return c.token }
func (c Credential) String() string         { return "[redacted]" }
func (c Credential) GoString() string       { return "[redacted]" }

// Identity combines effective application authority and an upstream credential.
// Scope/Prefix can be narrower than the underlying key, never broader.
type Identity struct {
	Subject    string
	Scope      api.Scope
	Prefix     string
	Credential Credential
}

// Resolver resolves the caller on EVERY invocation. Implementations must be
// concurrency safe. HTTP OAuth will use verified request context and a credential
// repository; stdio uses one verified key for the lifetime of the process.
type Resolver interface {
	Resolve(context.Context) (Identity, error)
}

type identityContextKey struct{}

// WithIdentity installs an identity verified by a trusted transport adapter.
// Never call this with values copied directly from tool arguments or headers.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, identity)
}

// ContextResolver resolves only a verified request identity. Unlike Static, it
// cannot fall back to a process credential when a caller has not authenticated.
type ContextResolver struct{}

func (ContextResolver) Resolve(ctx context.Context) (Identity, error) {
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	i, ok := ctx.Value(identityContextKey{}).(Identity)
	if !ok {
		return Identity{}, ErrForbidden
	}
	return i, nil
}

type Static struct{ Identity Identity }

func (s Static) Resolve(ctx context.Context) (Identity, error) {
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	return s.Identity, nil
}

var ErrForbidden = errors.New("operation is outside the caller's scope or path prefix")

// Require checks normalized paths. Listings may name an ancestor: their effective
// prefix must subsequently be narrowed with ListingPrefix before calling Vela.
func Require(i Identity, scope api.Scope, paths ...string) error {
	if !i.Scope.Allows(scope) {
		return ErrForbidden
	}
	for _, p := range paths {
		if i.Prefix != "" && !strings.HasPrefix(p, i.Prefix) {
			return ErrForbidden
		}
	}
	return nil
}

func ListingPrefix(i Identity, prefix string) (string, error) {
	if i.Prefix == "" || strings.HasPrefix(prefix, i.Prefix) {
		return prefix, nil
	}
	if strings.HasPrefix(i.Prefix, prefix) {
		return i.Prefix, nil
	}
	return "", ErrForbidden
}
