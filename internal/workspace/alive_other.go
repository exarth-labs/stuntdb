//go:build !unix && !windows

package workspace

// processAlive cannot check processes on this system, so it assumes they are
// alive. That only delays cleanup; it never removes a running run's files.
func processAlive(int) bool { return true }
