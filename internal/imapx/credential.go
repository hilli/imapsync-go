package imapx

import (
	"context"
)

// Credential answers with the secret to authenticate with.
//
// It is an interface rather than a string because a password and an access
// token are different kinds of thing. A password is a constant: resolved once,
// it stays true for the length of a run. A token is not — it lives about an
// hour, while a migration runs for several and the pool keeps dialling new
// connections throughout, on growth, on reconnection after an idle close, and
// on every retry. A secret resolved at startup and held is correct for the
// first and quietly wrong for the second.
type Credential interface {
	// Secret returns what to authenticate with now. It is called once per
	// dial, so an implementation that consults something slow or interactive
	// must cache.
	Secret(ctx context.Context) (string, error)

	// Refresh is called when the server refused stale. It answers with
	// something better, or reports that there is nothing better to be had.
	//
	// It is given the value that failed rather than being told to produce a
	// new one, and that does two jobs. A credential that cannot be renewed
	// answers no, so a wrong password is reported as a wrong password instead
	// of being asked for twice. And for one that can, comparing stale against
	// what is currently held is the whole of the single-flight: when a pool of
	// workers meets an expiry together they all offer the same failed value,
	// the first replaces it, and the rest find it already replaced and take
	// the replacement without minting anything.
	Refresh(ctx context.Context, stale string) (string, bool, error)

	// Mechanism names how the secret should be presented to the server.
	Mechanism() Mechanism
}

// Mechanism is the SASL mechanism, or LOGIN, that a credential is presented
// with. The credential decides: a token cannot be sent by LOGIN and a password
// has no business in an XOAUTH2 exchange, so a separate switch could only ever
// contradict the secret it applies to.
type Mechanism string

const (
	// MechanismLogin is the IMAP LOGIN command rather than a SASL mechanism.
	MechanismLogin Mechanism = "LOGIN"

	// MechanismXOAUTH2 is Google's and Microsoft's bearer-token mechanism.
	MechanismXOAUTH2 Mechanism = "XOAUTH2"
)

// StaticPassword is a credential that is already known and cannot be renewed.
func StaticPassword(password string) Credential { return staticPassword(password) }

type staticPassword string

func (p staticPassword) Secret(context.Context) (string, error) { return string(p), nil }

// Refresh always declines. A password the server has just rejected will be
// rejected again, so retrying would turn a clear failure into a slower one.
func (p staticPassword) Refresh(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func (p staticPassword) Mechanism() Mechanism { return MechanismLogin }
