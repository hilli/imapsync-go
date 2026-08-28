package probe

import (
	"context"
	"testing"

	"github.com/hilli/imapsync-go/internal/imapx"
)

// TestACancelledSearchReportsNoCeiling.
//
// Interrupting the probe stops it wherever it happened to be, which is not a
// measurement of anything. Reporting that count as a refusal would turn a
// Ctrl-C into advice: "this server allows four connections", written with the
// same confidence as a real wall and wrong by however much time was left.
//
// This is tested in-package because the only way to reach the path from
// outside is to race a cancellation against a live connection count, which
// would pick a different stopping point on every run.
func TestACancelledSearchReportsNoCeiling(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Dialling would fail on the cancelled context anyway, but the check at the
	// top of the loop means it is never reached: nothing is opened, so nothing
	// needs to be listening.
	held, why, refused := measureCeiling(ctx, imapx.DialOptions{}, 4)

	if refused {
		t.Error("a cancelled search found no ceiling, so nothing was refused")
	}
	if why != "probe cancelled" {
		t.Errorf("reason = %q, want it to say the probe was cancelled", why)
	}

	// The caller's own connection is the one thing that was genuinely open.
	if held != 1 {
		t.Errorf("held = %d, want 1", held)
	}

	r := Report{MaxConnections: held, Refused: refused}
	if got := r.SuggestedConcurrency(); got != held {
		t.Errorf("SuggestedConcurrency() = %d, want %d: with no ceiling found there is nothing to stay below", got, held)
	}
}
