package retry_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/retry"
)

func status(t imap.StatusResponseType, code imap.ResponseCode, text string) error {
	return &imap.Error{Type: t, Code: code, Text: text}
}

// TestClassify covers the failures a long run actually meets.
func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want retry.Kind
	}{
		{"cancelled by the caller", context.Canceled, retry.Stop},
		{"deadline passed", context.DeadlineExceeded, retry.Stop},
		{"bad credentials", status(imap.StatusResponseTypeNo, imap.ResponseCodeAuthenticationFailed, "nope"), retry.Stop},
		{"host does not exist", &net.DNSError{Err: "no such host", IsNotFound: true}, retry.Stop},

		{"message expunged", imapx.ErrMessageGone, retry.Skip},
		{"message too large", status(imap.StatusResponseTypeNo, imap.ResponseCodeTooBig, ""), retry.Skip},
		{"mailbox full", status(imap.StatusResponseTypeNo, imap.ResponseCodeOverQuota, ""), retry.Skip},
		{"command rejected", status(imap.StatusResponseTypeBad, "", "syntax error"), retry.Skip},
		{"nothing recognisable", errors.New("something new"), retry.Skip},

		{"connection desynchronised", imapx.ErrConnectionBroken, retry.Again},
		{"closed mid-response", io.ErrUnexpectedEOF, retry.Again},
		{"closed between responses", io.EOF, retry.Again},
		{"reset by peer", syscall.ECONNRESET, retry.Again},
		{"broken pipe", syscall.EPIPE, retry.Again},

		{"connection limit", status(imap.StatusResponseTypeNo, imap.ResponseCodeLimit, ""), retry.Slower},
		{"server unavailable", status(imap.StatusResponseTypeNo, imap.ResponseCodeUnavailable, ""), retry.Slower},
		{"iCloud, in words", status(imap.StatusResponseTypeNo, "", "Too many simultaneous connections"), retry.Slower},
		{"asked to come back later", status(imap.StatusResponseTypeNo, "", "Please try again later"), retry.Slower},
		{"nothing listening", syscall.ECONNREFUSED, retry.Slower},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := retry.Classify(tc.err); got != tc.want {
				t.Errorf("Classify(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyLooksThroughWrapping matters because every error in this codebase
// arrives wrapped in the context of where it happened. A classifier that only
// recognised bare sentinels would recognise nothing in practice.
func TestClassifyLooksThroughWrapping(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("appending to %q: %w", "INBOX",
		fmt.Errorf("copying literal: %w", imapx.ErrConnectionBroken))

	if got := retry.Classify(wrapped); got != retry.Again {
		t.Errorf("Classify(wrapped broken connection) = %v, want %v", got, retry.Again)
	}
}

// TestCancellationOutranksTheNetwork is the ordering this file exists to
// protect.
//
// Cancelling a run tears down connections underneath commands in flight, so the
// failures it produces are indistinguishable at the socket from the ones a
// flaky server produces. If the network check ran first, pressing Ctrl-C would
// be read as a blip and every worker would obediently reconnect and carry on.
func TestCancellationOutranksTheNetwork(t *testing.T) {
	t.Parallel()

	// A cancelled context reported as a connection reset, which is what a
	// closed socket looks like from inside a read.
	err := fmt.Errorf("%w: %w", syscall.ECONNRESET, context.Canceled)

	if got := retry.Classify(err); got != retry.Stop {
		t.Errorf("Classify(cancelled mid-read) = %v, want %v", got, retry.Stop)
	}
}

// timeoutError is a net.Error that has timed out, which is not otherwise
// constructible without a real network.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestATimedOutReadIsWorthRetrying(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("reading: %w", timeoutError{})
	if got := retry.Classify(err); got != retry.Again {
		t.Errorf("Classify(timeout) = %v, want %v", got, retry.Again)
	}
}

// longest samples the backoff repeatedly and reports the largest delay seen.
//
// One sample says nothing: with full jitter any single wait can be near zero
// whatever the ceiling is. The largest of twenty lands close enough to the
// ceiling to compare ceilings with.
func longest(t *testing.T, p retry.Policy, k retry.Kind, attempt int) time.Duration {
	t.Helper()

	var worst time.Duration
	for range 20 {
		start := time.Now()
		if err := p.Wait(context.Background(), k, attempt); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		worst = max(worst, time.Since(start))
	}
	return worst
}

// TestWaitBacksOffFurtherEachTime is what makes this backoff rather than a
// fixed pause. A server that has refused three times in a row is less likely to
// accept the fourth attempt than the second, so each wait has to be longer.
func TestWaitBacksOffFurtherEachTime(t *testing.T) {
	t.Parallel()

	p := retry.Policy{Attempts: 8, Base: 20 * time.Millisecond, Slow: time.Second, Max: time.Minute}

	first := longest(t, p, retry.Again, 0)
	fourth := longest(t, p, retry.Again, 3)

	// The ceilings are 20ms and 160ms, a factor of eight. Asserting only a
	// doubling leaves room for a slow machine to stretch the first sample.
	if fourth < 2*first {
		t.Errorf("longest wait was %v on the first attempt and %v on the fourth; the backoff is not growing",
			first, fourth)
	}
}

// TestWaitStaysUnderTheCeiling stops the growth above from running away.
//
// Doubling reaches half a second by the sixth attempt and half a minute by the
// eleventh. Without a cap a run that meets a long outage would spend hours
// asleep, having decided that the right response to a server being down is to
// stop asking whether it is still down.
func TestWaitStaysUnderTheCeiling(t *testing.T) {
	t.Parallel()

	// Attempt 6 would reach 640ms uncapped, which is 25 times the ceiling.
	p := retry.Policy{Attempts: 8, Base: 10 * time.Millisecond, Slow: time.Second, Max: 25 * time.Millisecond}

	if worst := longest(t, p, retry.Again, 6); worst > p.Max+75*time.Millisecond {
		t.Errorf("longest of twenty waits was %v, ceiling is %v", worst, p.Max)
	}
}

// TestWaitSpreadsRetriesOut guards the jitter.
//
// Every connection to a server that has just dropped them all fails at the same
// instant. Without jitter they would all sleep for exactly the same interval
// and arrive back together, which is the behaviour that turns a brief outage
// into a repeating one.
func TestWaitSpreadsRetriesOut(t *testing.T) {
	t.Parallel()

	p := retry.Policy{Attempts: 4, Base: 40 * time.Millisecond, Slow: time.Second, Max: time.Minute}

	const rounds = 12
	seen := make(map[time.Duration]bool, rounds)
	for range rounds {
		start := time.Now()
		if err := p.Wait(context.Background(), retry.Again, 2); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		// Rounded, so that scheduler noise is not mistaken for jitter.
		seen[time.Since(start).Round(10*time.Millisecond)] = true
	}

	if len(seen) < 3 {
		t.Errorf("%d rounds of backoff produced %d distinct delays (%v); retries are not being spread out",
			rounds, len(seen), seen)
	}
}

// TestSlowerWaitsLongerThanAgain is the whole reason the two are separate
// kinds: a throttled server is asking for less load, and answering it at
// connection-failure speed is how a throttle becomes a ban.
func TestSlowerWaitsLongerThanAgain(t *testing.T) {
	t.Parallel()

	p := retry.Policy{Attempts: 4, Base: 2 * time.Millisecond, Slow: 60 * time.Millisecond, Max: time.Minute}

	prompt := longest(t, p, retry.Again, 0)
	backedOff := longest(t, p, retry.Slower, 0)

	if backedOff < 4*prompt {
		t.Errorf("backing off waited at most %v and a prompt retry at most %v; a throttled server is being answered at full speed",
			backedOff, prompt)
	}
}

func TestWaitGivesUpWhenTheRunIsCancelled(t *testing.T) {
	t.Parallel()

	p := retry.Policy{Attempts: 4, Base: time.Hour, Slow: time.Hour, Max: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if err := p.Wait(ctx, retry.Slower, 3); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait on a cancelled context = %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("Wait took %v to notice cancellation", took)
	}
}
