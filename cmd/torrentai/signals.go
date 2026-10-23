package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// stopSignals end a run gracefully: SIGINT (Ctrl-C) and SIGTERM, which
// is how Kubernetes, systemd and most process supervisors ask a
// process to stop.
var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// signalError is the context cause when a signal stopped the run.
type signalError struct {
	sig os.Signal
}

func (e *signalError) Error() string {
	return "received " + e.sig.String()
}

// exitCode follows the shell convention of 128 plus the signal
// number: 130 for SIGINT, 143 for SIGTERM.
func (e *signalError) exitCode() int {
	if s, ok := e.sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return exitInterrupted
}

// signalName is a stable lowercase name for logs.
func (e *signalError) signalName() string {
	switch e.sig {
	case os.Interrupt:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	}
	return e.sig.String()
}

// withStopSignals returns a context cancelled, with a *signalError
// cause, when the first stop signal arrives. Handling is then reset,
// so a second signal kills the process at once if shutdown hangs.
func withStopSignals(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, stopSignals...)

	go func() {
		select {
		case s := <-sigs:
			signal.Reset(stopSignals...)
			cancel(&signalError{sig: s})
		case <-ctx.Done():
		}
		signal.Stop(sigs)
	}()

	return ctx, func() { cancel(nil) }
}
