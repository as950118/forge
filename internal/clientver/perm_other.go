//go:build !unix

package clientver

import "os"

func trustedOwner(os.FileInfo) (trusted, ok bool) { return true, false }

// systemGroups: no group information on this platform, so a group-writable
// location is never trusted.
type systemGroups struct{}

func (systemGroups) Trusted(os.FileInfo) bool { return false }
