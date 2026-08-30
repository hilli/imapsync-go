//go:build darwin

package localstore_test

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// birthtime reads a file's creation time, which is what Finder shows in its
// "Date Created" column.
func birthtime(t *testing.T, path string) (time.Time, bool) {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return time.Unix(st.Btim.Sec, st.Btim.Nsec), true
}
