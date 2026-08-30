//go:build !darwin

package localstore

import "time"

// setBirthtime is a no-op away from macOS and the BSDs.
//
// Linux records a creation time but offers no interface for setting one, so
// there the modification time carries the message's date alone. The folder
// database remains the authority on either platform.
func setBirthtime(string, time.Time) error { return nil }
