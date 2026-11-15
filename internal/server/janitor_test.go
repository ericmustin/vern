package server

import (
	"context"
	"testing"
	"time"
)

func TestRunJanitor_SweepsImmediatelyAndOnTick(t *testing.T) {
	logs := &logCapture{}

	// A long interval: only the immediate sweep happens.
	eng := &fakeEngine{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); RunJanitor(ctx, eng, 15*time.Minute, time.Hour, logs.logf) }()
	waitFor(t, "the immediate sweep", func() bool { return eng.sweepCount() == 1 })
	cancel()
	<-done
	eng.mu.Lock()
	if eng.sweeps[0] != 15*time.Minute {
		t.Errorf("Sweep olderThan = %s, want 15m", eng.sweeps[0])
	}
	eng.mu.Unlock()

	// A short interval: it keeps sweeping on every tick until ctx is done.
	eng2 := &fakeEngine{}
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { defer close(done2); RunJanitor(ctx2, eng2, time.Minute, 5*time.Millisecond, logs.logf) }()
	waitFor(t, "three sweeps", func() bool { return eng2.sweepCount() >= 3 })
	cancel2()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("RunJanitor did not return after ctx was cancelled")
	}
	n := eng2.sweepCount()
	time.Sleep(30 * time.Millisecond)
	if eng2.sweepCount() != n {
		t.Fatal("RunJanitor kept sweeping after it returned")
	}
}
