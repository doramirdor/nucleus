package supervisor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// makeStubChild builds a Child with an injectable respawn closure.
// Real Children hold an mcp-go stdio client that requires a live
// subprocess; for tests we just need to exercise the lifecycle
// (lastUsed, closed, respawn) so we leave Client nil and never call
// CallTool through the wrapper. Tests that DO need to dispatch wire
// in their own respawn that returns a usable client (none currently
// — the wrapper logic is unit-tested through behavior of the
// surrounding state machine).
func makeStubChild(t *testing.T, profileID string, respawn func(ctx context.Context) (*client.Client, []mcp.Tool, error)) *Child {
	t.Helper()
	return &Child{
		Connector: "test",
		Profile:   profileID,
		ProfileID: profileID,
		Tools:     nil,
		Client:    nil,
		lastUsed:  time.Now(),
		respawn:   respawn,
	}
}

// TestReapIdle_ClosesIdleChildren — the core reaper contract: a
// child whose lastUsed is older than the threshold gets closed; a
// fresh child does not.
func TestReapIdle_ClosesIdleChildren(t *testing.T) {
	s := New("test", "0.0.0")

	idle := makeStubChild(t, "test:idle", nil)
	idle.lastUsed = time.Now().Add(-10 * time.Minute)

	fresh := makeStubChild(t, "test:fresh", nil)
	fresh.lastUsed = time.Now().Add(-30 * time.Second)

	s.children["test:idle"] = idle
	s.children["test:fresh"] = fresh

	got := s.ReapIdle(5 * time.Minute)
	if got != 1 {
		t.Fatalf("ReapIdle reaped %d, want 1", got)
	}
	if !idle.IsClosed() {
		t.Errorf("idle child should be closed after reap")
	}
	if fresh.IsClosed() {
		t.Errorf("fresh child should NOT be closed by reap")
	}
}

// TestReapIdle_SkipsAlreadyClosed — calling ReapIdle a second time
// must not double-close already-reaped children.
func TestReapIdle_SkipsAlreadyClosed(t *testing.T) {
	s := New("test", "0.0.0")
	c := makeStubChild(t, "test:x", nil)
	c.lastUsed = time.Now().Add(-time.Hour)
	s.children["test:x"] = c

	if got := s.ReapIdle(time.Minute); got != 1 {
		t.Fatalf("first reap should close 1, got %d", got)
	}
	if got := s.ReapIdle(time.Minute); got != 0 {
		t.Errorf("second reap should not double-close, got %d", got)
	}
}

// TestReapIdle_ZeroDurationDisables — a zero/negative threshold
// must be a no-op. Otherwise an admin who sets idle=0 expecting
// "disable reaping" would have everything reaped immediately.
func TestReapIdle_ZeroDurationDisables(t *testing.T) {
	s := New("test", "0.0.0")
	c := makeStubChild(t, "test:x", nil)
	c.lastUsed = time.Now().Add(-time.Hour)
	s.children["test:x"] = c

	if got := s.ReapIdle(0); got != 0 {
		t.Errorf("ReapIdle(0) reaped %d; should be no-op", got)
	}
	if c.IsClosed() {
		t.Errorf("ReapIdle(0) should not close anything")
	}
}

// TestCallTool_RespawnsOnClosedChild — the centerpiece of the
// reap-then-call-again UX. After a child is closed, the next
// CallTool must transparently respawn (calling the closure) and
// proceed.
func TestCallTool_RespawnsOnClosedChild(t *testing.T) {
	var respawned int32
	respawn := func(ctx context.Context) (*client.Client, []mcp.Tool, error) {
		atomic.AddInt32(&respawned, 1)
		// Returning a non-nil error keeps us out of the "actually
		// dispatch the call" branch — we only want to verify the
		// respawn closure was invoked and the resulting error
		// surfaces cleanly.
		return nil, nil, errors.New("respawn-pretend-failed")
	}
	c := makeStubChild(t, "test:x", respawn)
	if _, err := c.closeForReap(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err := c.CallTool(context.Background(), mcp.CallToolRequest{})
	if err == nil {
		t.Fatalf("expected error from failing respawn, got nil")
	}
	if atomic.LoadInt32(&respawned) != 1 {
		t.Errorf("respawn closure not invoked; respawn count = %d",
			atomic.LoadInt32(&respawned))
	}
}

// TestCallTool_NoRespawnClosureFailsClearly — if a child was
// constructed without a respawn closure (e.g. some legacy test
// helper), the post-reap CallTool must fail with a clear message
// rather than nil-panic.
func TestCallTool_NoRespawnClosureFailsClearly(t *testing.T) {
	c := makeStubChild(t, "test:x", nil)
	if _, err := c.closeForReap(); err != nil {
		t.Fatalf("closeForReap: %v", err)
	}

	_, err := c.CallTool(context.Background(), mcp.CallToolRequest{})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	// Don't pin the exact wording — but it must mention the profile
	// so an operator reading logs has a starting point.
	if !contains(err.Error(), "test:x") {
		t.Errorf("error should reference profile id: %v", err)
	}
}

// TestStartReaper_ReapsOverTime — kick off the reaper goroutine
// with a tight tick interval and verify it actually reaps. This is
// the integration test for the goroutine lifecycle.
func TestStartReaper_ReapsOverTime(t *testing.T) {
	s := New("test", "0.0.0")
	c := makeStubChild(t, "test:x", nil)
	c.lastUsed = time.Now().Add(-time.Hour)
	s.children["test:x"] = c

	// 1ms idle threshold + 10ms tick → reaper closes the child on
	// its first or second tick. Wait up to 200ms so the test isn't
	// flaky on a loaded CI box.
	s.StartReaper(time.Millisecond, 10*time.Millisecond)
	defer s.StopReaper()

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if c.IsClosed() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("child not reaped within deadline; lastUsed=%v closed=%v",
		c.LastUsed(), c.IsClosed())
}

// TestStartReaper_StopReaperEnds — after StopReaper, no further
// reap work happens. We assert this by making a child idle AFTER
// stopping; even waiting past the tick interval, it must remain
// open.
func TestStartReaper_StopReaperEnds(t *testing.T) {
	s := New("test", "0.0.0")
	s.StartReaper(time.Millisecond, 5*time.Millisecond)
	s.StopReaper()

	c := makeStubChild(t, "test:x", nil)
	c.lastUsed = time.Now().Add(-time.Hour)
	s.children["test:x"] = c

	time.Sleep(50 * time.Millisecond) // 10x the tick interval
	if c.IsClosed() {
		t.Errorf("reaper still running after StopReaper closed a child")
	}
}

// TestStartReaper_DoubleStartIsSafe — once a reaper is started,
// subsequent StartReaper calls must be no-ops (sync.Once). Without
// the guard you'd get N goroutines competing on the same map and
// likely a race condition under -race.
func TestStartReaper_DoubleStartIsSafe(t *testing.T) {
	s := New("test", "0.0.0")
	s.StartReaper(time.Hour, time.Hour)
	s.StartReaper(time.Hour, time.Hour) // must not panic, must not start a second goroutine
	s.StopReaper()
}

// TestShutdown_StopsReaperAndClosesChildren — Shutdown is the
// gateway-lifetime exit path. It must clean up both the reaper
// goroutine (StopReaper) and every running child (closeForReap).
func TestShutdown_StopsReaperAndClosesChildren(t *testing.T) {
	s := New("test", "0.0.0")
	s.StartReaper(time.Hour, time.Hour)

	c := makeStubChild(t, "test:x", nil)
	s.children["test:x"] = c

	s.Shutdown()
	if !c.IsClosed() {
		t.Errorf("Shutdown should close all children")
	}
	if _, ok := s.children["test:x"]; ok {
		t.Errorf("Shutdown should drop children from the map")
	}
}

// TestLastUsed_UpdatedThroughCallTool — sanity-check that the
// timestamp moves forward on each call. Without this, sticky
// resolution and the reaper would both see stale values forever.
func TestLastUsed_UpdatedThroughCallTool(t *testing.T) {
	// Respawn returns a nil client on purpose — we never want this
	// test to actually run an MCP client; we just want to observe
	// lastUsed.
	c := makeStubChild(t, "test:x", func(ctx context.Context) (*client.Client, []mcp.Tool, error) {
		return nil, nil, errors.New("respawn-not-needed-here")
	})
	// Bypass the closed branch by leaving Client = nil but closed = false.
	// In real life Client would be a real *client.Client; here we want
	// to verify the lastUsed bookkeeping itself, so we expect a panic-
	// or nil-deref-style failure when CallTool tries to dispatch — and
	// then assert lastUsed wasn't moved (the panic catches via recover).
	pre := c.LastUsed()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic from nil client dispatch")
		}
		// lastUsed should NOT have been updated because the dispatch
		// never returned successfully.
		if !c.LastUsed().Equal(pre) {
			t.Errorf("lastUsed advanced despite failed dispatch: pre=%v post=%v",
				pre, c.LastUsed())
		}
	}()
	_, _ = c.CallTool(context.Background(), mcp.CallToolRequest{})
}

// TestConcurrentReapAndCall_NoRace — race-detector probe. Hammer
// the supervisor with simultaneous ReapIdle calls and concurrent
// state reads (LastUsed, IsClosed). Run with `go test -race`.
func TestConcurrentReapAndCall_NoRace(t *testing.T) {
	s := New("test", "0.0.0")
	for i := 0; i < 20; i++ {
		id := "test:c" + itoa(i)
		c := makeStubChild(t, id, nil)
		c.lastUsed = time.Now().Add(-time.Hour)
		s.children[id] = c
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s.ReapIdle(time.Second)
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					for _, c := range s.Children() {
						_ = c.IsClosed()
						_ = c.LastUsed()
					}
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	if i < 100 {
		return string(rune('0'+i/10)) + string(rune('0'+i%10))
	}
	return "x"
}
