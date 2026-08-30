//go:build darwin

package localstore

import (
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// setBirthtime sets a file's creation time.
//
// This is the timestamp Finder shows in its "Date Created" column, and
// os.Chtimes cannot reach it. Without this a store would list every message as
// created on the day of the backup, which is exactly the information a person
// browsing the store is looking for.
func setBirthtime(path string, t time.Time) error {
	attrs := unix.Attrlist{
		Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr:  unix.ATTR_CMN_CRTIME,
	}
	ts := unix.NsecToTimespec(t.UnixNano())
	//nolint:gosec // Setattrlist takes the attribute value as raw bytes; there is no other way to pass a timespec
	buf := (*[unsafe.Sizeof(ts)]byte)(unsafe.Pointer(&ts))[:]
	return unix.Setattrlist(path, &attrs, buf, 0)
}
