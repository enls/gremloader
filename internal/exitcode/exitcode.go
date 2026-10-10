// Package exitcode defines the process exit codes. They are part of the command-line contract:
// scripts and the scheduler branch on them, so they are stable and never reused.
package exitcode

const (
	// OK means the command did what was asked.
	OK = 0
	// Failed is a runtime failure: something was attempted and did not work.
	Failed = 1
	// UpdatesAvailable is returned by check-style commands when nothing failed but there is
	// something to update. It is not an error.
	UpdatesAvailable = 2
	// Locked means another GremLoader run holds the lock.
	Locked = 3
	// Usage means the command line was wrong; nothing was attempted.
	Usage = 4
)
