//go:build !windows

package outfile

// transient reports whether a rename failed for a reason that usually passes.
// Outside Windows, an open file never blocks a rename.
func transient(error) bool { return false }
