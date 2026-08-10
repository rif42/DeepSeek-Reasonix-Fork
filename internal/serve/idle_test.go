package serve

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// waitFired polls until the fire hook ran or the deadline passes.
func waitFired(t *testing.T, fired *atomic.Bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !fired.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("idle shutdown never fired (%s)", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestIdleShutdownFiresAfterGrace(t *testing.T) {
	var fired atomic.Bool
	id := newIdleShutdown(120 * time.Millisecond)
	id.fireHook = func() { fired.Store(true) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id.start(ctx, func() bool { return false })
	defer id.Stop()
	waitFired(t, &fired, "no clients, no turn")
}

func TestIdleShutdownDisarmedByClient(t *testing.T) {
	var fired atomic.Bool
	id := newIdleShutdown(100 * time.Millisecond)
	id.fireHook = func() { fired.Store(true) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id.start(ctx, func() bool { return false })
	defer id.Stop()

	id.clientOpened()
	time.Sleep(350 * time.Millisecond)
	if fired.Load() {
		t.Fatal("fired while a client was connected")
	}
	id.clientClosed()
	waitFired(t, &fired, "after last client closed")
}

func TestIdleShutdownDeferredWhileRunning(t *testing.T) {
	var fired atomic.Bool
	id := newIdleShutdown(100 * time.Millisecond)
	id.fireHook = func() { fired.Store(true) }
	running := atomic.Bool{}
	running.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id.start(ctx, func() bool { return running.Load() })
	defer id.Stop()

	time.Sleep(350 * time.Millisecond)
	if fired.Load() {
		t.Fatal("fired while a turn was running")
	}
	running.Store(false)
	waitFired(t, &fired, "after turn finished")
}
