package serve

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// idleShutdown implements the "close the tab, instance lingers, then exits"
// rule: when enabled, a monitor arms a shutdown timer once the last SSE client
// disconnects and no turn is running. A new client disarms the timer; a
// running turn defers it (the monitor re-arms on its next tick after the turn
// ends). The fire is one-shot — the server is exiting.
type idleShutdown struct {
	enabled time.Duration // 0 = disabled
	clients atomic.Int64

	monitorC context.CancelFunc
	mu       sync.Mutex
	timer    *time.Timer
	fireOnce sync.Once
	shutdown chan struct{}
	// fireHook short-circuits the real shutdown path in tests; onFire runs
	// server-side pre-shutdown work (snapshot, registry cleanup).
	fireHook func()
	onFire   func()
}

func newIdleShutdown(grace time.Duration) *idleShutdown {
	return &idleShutdown{enabled: grace, shutdown: make(chan struct{})}
}

func (id *idleShutdown) clientOpened() { id.clients.Add(1) }
func (id *idleShutdown) clientClosed() { id.clients.Add(-1) }

// start launches the monitor; canceling ctx stops it. running reports whether
// a turn is in flight and must defer the shutdown.
func (id *idleShutdown) start(ctx context.Context, running func() bool) {
	if id == nil || id.enabled <= 0 {
		return
	}
	mctx, cancel := context.WithCancel(ctx)
	id.monitorC = cancel
	go id.monitor(mctx, running)
}

func (id *idleShutdown) monitor(ctx context.Context, running func() bool) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			id.mu.Lock()
			id.stopTimerLocked()
			id.mu.Unlock()
			return
		case <-ticker.C:
			if id.clients.Load() > 0 || running() {
				id.mu.Lock()
				if id.timer != nil {
					id.timer.Stop()
					id.timer = nil
				}
				id.mu.Unlock()
				continue
			}
			id.mu.Lock()
			if id.timer == nil {
				id.timer = time.AfterFunc(id.enabled, id.fire)
			}
			id.mu.Unlock()
		}
	}
}

func (id *idleShutdown) stopTimerLocked() {
	if id.timer != nil {
		id.timer.Stop()
		id.timer = nil
	}
}

// fire triggers the one-shot shutdown, running server-side pre-work first.
func (id *idleShutdown) fire() {
	id.fireOnce.Do(func() {
		if id.fireHook != nil {
			id.fireHook()
			return
		}
		if id.onFire != nil {
			id.onFire()
		}
		slog.Info("serve: idle shutdown — no open tabs", "grace", id.enabled)
		close(id.shutdown)
	})
}

// ShutdownCh is closed when idle shutdown fires (nil when disabled).
func (id *idleShutdown) ShutdownCh() <-chan struct{} {
	if id == nil || id.enabled <= 0 {
		return nil
	}
	return id.shutdown
}

// Stop halts the monitor and timer (server teardown / tests).
func (id *idleShutdown) Stop() {
	if id.monitorC != nil {
		id.monitorC()
	}
}
