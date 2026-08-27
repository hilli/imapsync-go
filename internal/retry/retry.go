// Package retry decides what to do about a failure, and how long to wait
// before doing it.
//
// A sync of a large account runs for hours against a server nobody controls.
// Over that span a dropped connection is not an exceptional event, it is a
// scheduled one: TLS sessions expire, load balancers recycle, and throttles
// engage. The difference between a run that finishes unattended and one that
// has to be nursed is entirely in how those are handled.
//
// The classification has four outcomes rather than a boolean because "should I
// retry?" is the wrong question. Retrying an expunged message never works;
// retrying a throttled one works only if the retry is slower than the last
// attempt; and a cancelled context must not be retried at all, however
// transient it looks.
package retry

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/hilli/imapsync-go/internal/imapx"
)

// Kind is what to do about a failure.
type Kind int

const (
	// Stop means abandon the run. The caller cancelled it, or credentials are
	// wrong, and no amount of waiting changes either.
	Stop Kind = iota

	// Skip means this message will not copy, but the rest of the folder can.
	// The message is recorded with its reason and picked up by a later run.
	Skip

	// Again means try once more on a fresh connection, after a short pause.
	Again

	// Slower means try once more, but give the server room first. It is Again
	// with a longer fuse, kept separate because a server that says "too many
	// connections" is asking for less load, and answering it with a prompt
	// retry is how a throttle becomes a ban.
	Slower
)

func (k Kind) String() string {
	switch k {
	case Stop:
		return "stop"
	case Skip:
		return "skip"
	case Again:
		return "retry"
	case Slower:
		return "back off"
	}
	return "unknown"
}

// Classify says what to do about err.
//
// The default for an unrecognised error is Skip rather than Again. Guessing
// "transient" on an unknown failure risks an unbounded loop against a server
// doing something this code has never seen; guessing "permanent" costs one
// warning line and a retry on the next run, because only messages recorded as
// done are skipped when a run is repeated. The cheaper mistake wins.
func Classify(err error) Kind {
	if err == nil {
		return Skip
	}

	// Cancellation first, and before anything else: a cancelled context
	// surfaces as a read error on whatever connection was in flight, so asking
	// the network questions first would classify the user pressing Ctrl-C as a
	// broken connection and dutifully retry it.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Stop
	}

	if errors.Is(err, imapx.ErrMessageGone) {
		return Skip
	}

	// A desynchronised connection is the ordinary way a long run is
	// interrupted. The connection itself is unusable and the pool discards it,
	// but the operation is fine on a new one.
	if errors.Is(err, imapx.ErrConnectionBroken) {
		return Again
	}

	if k, ok := classifyStatus(err); ok {
		return k
	}
	if k, ok := classifyNetwork(err); ok {
		return k
	}
	return Skip
}

// classifyStatus reads the server's own account of what went wrong.
//
// Response codes are far better evidence than the human-readable text beside
// them, which is unstandardised and varies between servers and versions. The
// text is consulted only when there is no code, which on older servers is most
// of the time.
func classifyStatus(err error) (Kind, bool) {
	var status *imap.Error
	if !errors.As(err, &status) {
		return 0, false
	}

	switch status.Code {
	case imap.ResponseCodeAuthenticationFailed,
		imap.ResponseCodeAuthorizationFailed,
		imap.ResponseCodePrivacyRequired,
		imap.ResponseCodeExpired:
		// Every connection to this account will be refused the same way, so
		// there is nothing for the rest of the run to do.
		return Stop, true

	case imap.ResponseCodeLimit,
		imap.ResponseCodeInUse,
		imap.ResponseCodeTooMany,
		imap.ResponseCodeUnavailable,
		imap.ResponseCodeContactAdmin:
		return Slower, true

	case imap.ResponseCodeOverQuota:
		// The destination is full. Retrying this message cannot help, and the
		// next one is no more likely to fit, but the run is left to discover
		// that rather than assuming every remaining message is too big.
		return Skip, true

	case imap.ResponseCodeTooBig,
		imap.ResponseCodeNonExistent,
		imap.ResponseCodeNoPerm,
		imap.ResponseCodeCannot,
		imap.ResponseCodeAlreadyExists,
		imap.ResponseCodeTryCreate,
		imap.ResponseCodeCorruption,
		imap.ResponseCodeServerBug,
		imap.ResponseCodeClientBug,
		imap.ResponseCodeParse:
		return Skip, true
	}

	// BAD means the server rejected the command itself. Sending it again
	// unchanged produces the same answer.
	if status.Type == imap.StatusResponseTypeBad {
		return Skip, true
	}
	if kind, ok := classifyText(status.Text); ok {
		return kind, true
	}
	return Skip, true
}

// throttleWords are the ways servers say "slow down" when they send no response
// code for it. iCloud's is the first.
var throttleWords = []string{
	"too many simultaneous",
	"too many connections",
	"maximum number of connections",
	"try again later",
	"try later",
	"temporarily unavailable",
	"server busy",
	"rate limit",
	"throttl",
	"exceeded the limit",
}

func classifyText(text string) (Kind, bool) {
	lower := strings.ToLower(text)
	for _, w := range throttleWords {
		if strings.Contains(lower, w) {
			return Slower, true
		}
	}
	return 0, false
}

// classifyNetwork covers the failures that never reach the IMAP layer at all.
func classifyNetwork(err error) (Kind, bool) {
	// An unexpected EOF is a connection closed mid-response; a plain EOF is one
	// closed between them. Both mean the peer went away, which a new connection
	// fixes.
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return Again, true
	}
	if errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT) {
		return Again, true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) {
		// The server is not accepting connections at all. That is usually a
		// restart or a load balancer withdrawing a backend, and worth waiting
		// out rather than reconnecting into immediately.
		return Slower, true
	}

	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return Again, true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.IsNotFound {
			return Stop, true
		}
		return Slower, true
	}
	return 0, false
}

// Policy is how many attempts a failure gets and how long the pauses between
// them are.
type Policy struct {
	// Attempts is the total number of tries, so 1 means no retry at all.
	Attempts int
	// Base is the first pause after a connection failure.
	Base time.Duration
	// Slow is the first pause when the server has asked for less load.
	Slow time.Duration
	// Max caps any single pause, because exponential growth is only useful
	// until it exceeds the time anyone is willing to spend on one message.
	Max time.Duration
}

// Default is the policy a run uses unless told otherwise.
//
// Four attempts spans a few seconds of connection trouble and about half a
// minute of throttling, which covers a server restart or a load balancer moving
// a connection. Beyond that the failure is not a blip, and continuing to try one
// message is less useful than recording it and moving to the next: the state
// database means a later run picks it up for free.
func Default() Policy {
	return Policy{Attempts: 4, Base: 500 * time.Millisecond, Slow: 5 * time.Second, Max: time.Minute}
}

// Wait pauses before attempt number n, counting the first attempt as zero.
//
// The delay is exponential with full jitter: a random duration between zero and
// the current ceiling, rather than the ceiling itself. When a server drops
// connections it drops all of them at once, so every worker fails at the same
// instant. Backing off by a fixed amount reassembles them into a queue that
// arrives at the recovering server together, and again after the next doubling.
// Spreading each attempt across the whole interval takes the herd apart.
func (p Policy) Wait(ctx context.Context, k Kind, attempt int) error {
	base := p.Base
	if k == Slower {
		base = p.Slow
	}

	ceiling := base << min(attempt, 20)
	if ceiling > p.Max || ceiling <= 0 {
		ceiling = p.Max
	}

	t := time.NewTimer(rand.N(ceiling + 1))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
