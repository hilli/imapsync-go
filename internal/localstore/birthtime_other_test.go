//go:build !darwin

package localstore_test

import (
	"testing"
	"time"
)

// birthtime reports that this platform has no creation time to check. Linux
// records one but offers no interface for setting it, so there the
// modification time carries the message's date alone.
func birthtime(*testing.T, string) (time.Time, bool) { return time.Time{}, false }
