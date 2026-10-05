package fxmodules

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/fx"
)

// Test_aWorkerOutlivesItsStartHookContext is the regression test for a bug this
// suite could not see.
//
// Every worker used to be given the context Fx passes to OnStart. That context
// is a child of the context handed to app.Start, so it carries the start timeout
// and is cancelled when the hook returns. A worker running on it stopped working
// while the process kept serving traffic and kept answering /health — an
// instance that looked healthy and had published nothing for a minute.
//
// The bug survived the integration suite because those tests finish in seconds,
// well inside the sixty-second window. What matters is not that the loop starts
// but that it is still running long after the hook that started it is gone, so
// that is what this asserts.
func Test_aWorkerOutlivesItsStartHookContext(t *testing.T) {
	loop := newCountingLoop()

	// The start context carries a short deadline, standing in for the
	// sixty-second timeout cmd/worker uses. If the loop were bound to it, the
	// deadline would stop the worker.
	app := fx.New(
		fx.NopLogger,
		fx.Invoke(func(lc fx.Lifecycle) { run(lc, "test_worker", discardLogger(), loop.run) }),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("starting the app: %v", err)
	}
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	// Let the start context expire, which is what used to kill the worker.
	<-ctx.Done()
	time.Sleep(50 * time.Millisecond)

	if stopped := loop.stopped.Load(); stopped != 0 {
		t.Fatal("the worker's context was cancelled when its start hook returned; " +
			"a worker must run on a context tied to the application, not to start-up")
	}
	if loops := loop.started.Load(); loops != 1 {
		t.Fatalf("the loop ran %d times, want 1", loops)
	}
}

// Test_aWorkerIsWaitedForOnStop covers the other half of the promise: the stop
// hook does not return until the loop has returned, so a shutdown cannot cut a
// worker off mid-batch.
func Test_aWorkerIsWaitedForOnStop(t *testing.T) {
	loop := newCountingLoop()

	app := fx.New(
		fx.NopLogger,
		fx.Invoke(func(lc fx.Lifecycle) { run(lc, "test_worker", discardLogger(), loop.run) }),
	)
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("starting the app: %v", err)
	}
	if !loop.waitStarted(t) {
		t.Fatal("the loop never started")
	}

	stopped := make(chan struct{})
	go func() {
		_ = app.Stop(context.Background())
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within five seconds; the loop is not being waited for")
	}

	if loop.stopped.Load() == 0 {
		t.Fatal("Stop returned before the loop returned, so a shutdown can abandon work in flight")
	}
}

// countingLoop stands in for a worker: it records that it started, records that
// it stopped, and returns only when its context is cancelled.
type countingLoop struct {
	started   atomic.Int64
	stopped   atomic.Int64
	began     chan struct{}
	beganOnce chan struct{}
}

func newCountingLoop() *countingLoop {
	return &countingLoop{began: make(chan struct{}), beganOnce: make(chan struct{})}
}

func (l *countingLoop) run(ctx context.Context) error {
	l.started.Add(1)
	select {
	case l.began <- struct{}{}:
	default:
		close(l.beganOnce)
	}
	<-ctx.Done()
	l.stopped.Add(1)
	return nil
}

// waitStarted blocks until the loop reports it is running.
func (l *countingLoop) waitStarted(t *testing.T) bool {
	t.Helper()
	select {
	case <-l.began:
	case <-l.beganOnce:
	case <-time.After(5 * time.Second):
		return false
	}
	return true
}

// discardLogger keeps the test output free of the errors run() logs on purpose.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
