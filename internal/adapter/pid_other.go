//go:build !unix

package adapter

// processAlive: without a portable liveness probe leftovers are treated as stale
// (the sync lock already guarantees that no other Forge sync is running).
func processAlive(int) bool { return false }
