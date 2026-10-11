package cli

import (
	"context"
	"io"
	"os"

	"github.com/enls/gremloader/internal/exitcode"
)

// interruptible turns interrupts into the behaviour of plan P3-12. The first one cancels the
// returned context: the engine stops between packages, and a change in progress is undone so
// that every install is as it was or as it should be. The second one quits at once through exit;
// the journal makes that safe, and `gremloader repair` finishes or undoes what was in progress.
// stop ends the watch (call it when the run is over).
//
// Signals arrive on sig, so tests can send them; Run connects it to the real ones.
func interruptible(sig <-chan os.Signal, stderr io.Writer, exit func(int)) (ctx context.Context, stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
		case <-done:
			return
		}
		say(stderr, "\ninterrupted: stopping after the current step, which is finished or undone first "+
			"(interrupt again to quit at once)\n")
		cancel()
		select {
		case <-sig:
		case <-done:
			return
		}
		say(stderr, "\nquitting at once; if a change was in progress, `gremloader repair` finishes or undoes it\n")
		exit(exitcode.Failed)
	}()
	return ctx, func() { close(done); cancel() }
}
