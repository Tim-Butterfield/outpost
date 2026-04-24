// Package auth defines the submitter-identity abstraction.
//
// File-RPC does not authenticate: filesystem ACLs on the shared
// directory are the authorization boundary. The NoOp implementation
// provided here is what ships. The interface exists so other
// transports can add real authentication if they need it.
package auth

import "context"

// Authenticator decides whether the current caller is allowed to
// submit to a responder. The interface is intentionally narrow;
// the caller provides no arguments, letting each implementation
// gather credentials from wherever it needs (env, keyring, file).
//
// A nil-returning Authorize means "permitted". Any non-nil error
// means "denied" and must be propagated back to the caller
// unchanged so the submitter can surface auth failures clearly.
type Authenticator interface {
	Authorize(ctx context.Context) error
}

// NoOp returns an Authenticator that always permits. This is the
// default for file-RPC, where access control lives in the
// underlying filesystem.
func NoOp() Authenticator { return noopAuth{} }

type noopAuth struct{}

func (noopAuth) Authorize(context.Context) error { return nil }
