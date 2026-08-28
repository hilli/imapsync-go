package pool

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/hilli/imapsync-go/internal/imapx"
)

// These tests are in-package because the question they ask is about the clock,
// and moving the clock without sleeping means reaching for a field that has no
// business in the exported Options. The behaviour they cover is otherwise
// untestable: waiting out a ten-second window in a unit test is not a test, it
// is a delay.

func poolForClock(t *testing.T, capacity int) *Pool {
	t.Helper()

	p, err := New(Options{
		Cap:  capacity,
		Dial: func(context.Context) (imapx.Conn, error) { return nil, errors.New("not dialled") },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return p
}

// TestARefusalIsJudgedByWhatElseIsWorking.
//
// The whole design rests on this distinction and neither side of it is visible
// in the error. A server at its connection limit hangs up on the one connection
// too many while serving all the others; a server that has restarted fails
// everything at once. Both arrive as an unexpected EOF.
func TestARefusalIsJudgedByWhatElseIsWorking(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name      string
		lastOK    time.Duration // how long before now, or 0 for never
		everOK    bool
		wantLimit bool
	}{
		{"another connection just finished work", 0, true, true},
		{"just inside the window", capacityWindow - time.Second, true, true},
		{"exactly at the window", capacityWindow, true, true},
		{"just outside the window", capacityWindow + time.Second, true, false},
		{"nothing has ever succeeded", 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := poolForClock(t, 8)
			p.now = func() time.Time { return now }
			p.open = 4
			if tc.everOK {
				p.lastOK = now.Add(-tc.lastOK)
			}

			if got := p.refused(io.ErrUnexpectedEOF); got != tc.wantLimit {
				t.Errorf("refused() = %v, want %v", got, tc.wantLimit)
			}

			wantWidth := 8
			if tc.wantLimit {
				wantWidth = 4
			}
			if got := p.Width(); got != wantWidth {
				t.Errorf("Width() = %d, want %d", got, wantWidth)
			}
		})
	}
}

// TestAServerRestartDoesNotShrinkThePool is the false positive the window
// exists to prevent, played out rather than asserted a field at a time.
//
// When a server restarts, every connection dies and every worker reconnects, so
// dial failures arrive in a crowd that looks exactly like a connection limit.
// Shrinking on them would be permanent — the width never rises — so a fault
// lasting seconds would cost the rest of a run lasting hours.
func TestAServerRestartDoesNotShrinkThePool(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	p := poolForClock(t, 16)
	p.now = func() time.Time { return now }
	p.open = 16
	p.lastOK = now // healthy a moment ago, as it would have been

	// The server goes away. Work stops succeeding, and the clock moves past the
	// window while every worker fails to reconnect.
	now = now.Add(capacityWindow + time.Second)

	for range 16 {
		if p.refused(io.ErrUnexpectedEOF) {
			t.Fatal("a dial failure during a restart was read as a connection limit")
		}
	}
	if got := p.Width(); got != 16 {
		t.Errorf("Width() = %d after a restart, want the pool untouched at 16", got)
	}
}

// TestAHerdOfRefusalsShrinksThePoolOnce.
//
// A wall is met by every worker in the same instant, so one refusal arrives per
// worker. If each recomputed a width from a falling count they would walk the
// pool to its floor over a single event, which was the failure most expected of
// this design.
//
// It does not happen, and the reason is worth stating because it is not a
// mechanism anybody added: all thirty read the same number of open connections,
// so all thirty compute the same width, and twenty-nine of them find the pool
// already there.
func TestAHerdOfRefusalsShrinksThePoolOnce(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	p := poolForClock(t, 30)
	p.now = func() time.Time { return now }
	p.open = 12
	p.lastOK = now

	for range 30 {
		if !p.refused(io.ErrUnexpectedEOF) {
			t.Fatal("a refusal at the wall was not recognised")
		}
	}

	if got := p.Width(); got != 12 {
		t.Errorf("Width() = %d after thirty simultaneous refusals, want 12", got)
	}
}

// TestAFurtherRefusalAfterSettlingShrinksAgain.
//
// The cooldown must not become a permanent deafness. The limit is shared with
// whatever else is talking to the server, so it moves: another client taking
// connections while we shrank is new information, not an echo.
func TestAFurtherRefusalAfterSettlingShrinksAgain(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	p := poolForClock(t, 30)
	p.now = func() time.Time { return now }
	p.open = 12
	p.lastOK = now

	if !p.refused(io.ErrUnexpectedEOF) {
		t.Fatal("first refusal not recognised")
	}
	if got := p.Width(); got != 12 {
		t.Fatalf("Width() = %d, want 12", got)
	}

	// The shrink is applied: the debt was payable from free tokens, since
	// nothing is leased here.
	p.mu.Lock()
	settled := p.debt == 0 && p.open <= p.width
	p.mu.Unlock()
	if !settled {
		t.Fatal("shrink did not settle from free tokens")
	}

	p.open = 6
	if !p.refused(io.ErrUnexpectedEOF) {
		t.Fatal("second refusal not recognised")
	}
	if got := p.Width(); got != 6 {
		t.Errorf("Width() = %d after a second, genuinely new refusal, want 6", got)
	}
}

// TestTheWidthNeverReachesZero. A pool of width zero can do nothing at all and
// would never recover, since nothing here grows. One connection is slow; none is
// a hang.
func TestTheWidthNeverReachesZero(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	p := poolForClock(t, 4)
	p.now = func() time.Time { return now }
	p.open = 0
	p.lastOK = now

	if !p.refused(io.ErrUnexpectedEOF) {
		t.Fatal("refusal with nothing open was not recognised as the limit")
	}
	if got := p.Width(); got != 1 {
		t.Errorf("Width() = %d, want a floor of 1", got)
	}
}
