package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// These tests cover baseRouter's own machinery — the run loop, process
// lifecycle (doSwap), grant/ServeHTTP plumbing, Unload, and Shutdown. The
// scheduling decision logic (queueing, collation, eviction collisions) lives in
// the scheduler package and is tested directly there; see fifo_test.go.

// stubPlanner evicts configured targets. baseRouter tests drive the run loop
// through the default FIFO scheduler without exercising router planner details.
type stubPlanner struct {
	evict map[string][]string
}

func (s *stubPlanner) EvictionFor(target string, _ []string) []string {
	if s.evict == nil {
		return nil
	}
	return s.evict[target]
}
func (s *stubPlanner) OnSwapStart(string, []string) {}

func newTestBase(t *testing.T, processes map[string]process.Process, planner scheduler.Swapper) *baseRouter {
	t.Helper()
	conf := config.Config{HealthCheckTimeout: 5}
	return newTestBaseWithConfig(t, conf, processes, planner)
}

func newTestBaseWithConfig(t *testing.T, conf config.Config, processes map[string]process.Process, planner scheduler.Swapper) *baseRouter {
	t.Helper()
	b, err := newBaseRouter("test", conf, processes, logmon.NewWriter(io.Discard), planner)
	if err != nil {
		t.Fatalf("newBaseRouter: %v", err)
	}
	b.testProcessed = make(chan struct{}, 64)
	go b.run()
	t.Cleanup(func() {
		if !b.shuttingDown.Load() {
			_ = b.Shutdown(time.Second)
		}
	})
	return b
}

func TestBaseRouter_RunningModels(t *testing.T) {
	ready := newFakeProcess("ready")
	ready.markReady()
	starting := newFakeProcess("starting")
	starting.setState(process.StateStarting)
	stopped := newFakeProcess("stopped")

	b := newTestBase(t, map[string]process.Process{
		"ready": ready, "starting": starting, "stopped": stopped,
	}, &stubPlanner{})

	running := b.RunningModels()
	if len(running) != 2 {
		t.Fatalf("running=%v want 2 entries", running)
	}
	if running["ready"] != process.StateReady {
		t.Errorf("ready state=%q want ready", running["ready"])
	}
	if running["starting"] != process.StateStarting {
		t.Errorf("starting state=%q want starting", running["starting"])
	}
	if _, ok := running["stopped"]; ok {
		t.Errorf("stopped process should be excluded from RunningModels")
	}
}

func TestBaseRouter_UnloadAll(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	c := newFakeProcess("c")
	c.markReady()

	b := newTestBase(t, map[string]process.Process{"a": a, "c": c}, &stubPlanner{})
	b.Unload(time.Second)

	if a.State() != process.StateStopped || c.State() != process.StateStopped {
		t.Fatalf("Unload() should stop every process: a=%q c=%q", a.State(), c.State())
	}
}

func TestBaseRouter_UnloadSpecificModel(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	c := newFakeProcess("c")
	c.markReady()

	b := newTestBase(t, map[string]process.Process{"a": a, "c": c}, &stubPlanner{})
	b.Unload(time.Second, "a")

	if a.State() != process.StateStopped {
		t.Errorf("a should be stopped, got %q", a.State())
	}
	if c.State() != process.StateReady {
		t.Errorf("c should remain ready, got %q", c.State())
	}
}

func TestBaseRouter_UnloadSpecificModelUsesConfiguredTimeout(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	c := newFakeProcess("c")
	c.markReady()

	conf := config.Config{
		HealthCheckTimeout: 5,
		UnloadTimeout:      25,
		Models: map[string]config.ModelConfig{
			"a": {UnloadTimeout: 45},
			"c": {UnloadTimeout: 25},
		},
	}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a, "c": c}, &stubPlanner{})
	b.Unload(0, "a")

	if a.lastStopTimeout() != 45*time.Second {
		t.Errorf("a stop timeout=%v want 45s", a.lastStopTimeout())
	}
	if got := c.stopCalls.Load(); got != 0 {
		t.Errorf("c stopCalls=%d want 0", got)
	}
}

func TestBaseRouter_UnloadAllUsesConfiguredTimeouts(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	c := newFakeProcess("c")
	c.markReady()

	conf := config.Config{
		HealthCheckTimeout: 5,
		UnloadTimeout:      25,
		Models: map[string]config.ModelConfig{
			"a": {UnloadTimeout: 45},
			"c": {UnloadTimeout: 25},
		},
	}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a, "c": c}, &stubPlanner{})
	b.Unload(0)

	if a.lastStopTimeout() != 45*time.Second {
		t.Errorf("a stop timeout=%v want 45s", a.lastStopTimeout())
	}
	if c.lastStopTimeout() != 25*time.Second {
		t.Errorf("c stop timeout=%v want 25s", c.lastStopTimeout())
	}
}

func TestBaseRouter_UnloadStopsSmallestTimeoutFirst(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	c := newFakeProcess("c")
	c.markReady()
	e := newFakeProcess("e")
	e.markReady()

	var mu sync.Mutex
	var order []string
	record := func(id string) {
		mu.Lock()
		order = append(order, id)
		mu.Unlock()
	}
	a.onStop = record
	c.onStop = record
	e.onStop = record

	conf := config.Config{
		HealthCheckTimeout: 5,
		UnloadTimeout:      25,
		Models: map[string]config.ModelConfig{
			"a": {UnloadTimeout: 45},
			"c": {UnloadTimeout: 10},
			"e": {UnloadTimeout: 25},
		},
	}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a, "c": c, "e": e}, &stubPlanner{})
	// Named in descending timeout order; Unload must re-order ascending.
	b.Unload(0, "a", "e", "c")

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"c", "e", "a"}; !slices.Equal(order, want) {
		t.Errorf("stop order=%v want %v", order, want)
	}
}

// TestBaseRouter_UnloadZeroStopsSameTimeoutInParallel verifies that models
// resolving to the same unloadTimeout share one unload request and stop
// concurrently, rather than one request per model. Both fakeProcess.Stop
// calls are pinned via stopBlock; the test only releases them after
// observing both stopStarted, which deadlocks if the stops were sequential.
func TestBaseRouter_UnloadZeroStopsSameTimeoutInParallel(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	a.stopBlock = make(chan struct{})
	c := newFakeProcess("c")
	c.markReady()
	c.stopBlock = make(chan struct{})

	conf := config.Config{
		HealthCheckTimeout: 5,
		UnloadTimeout:      25,
		// no per-model values: both models inherit the global 25s
	}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a, "c": c}, &stubPlanner{})

	unloadDone := make(chan struct{})
	go func() {
		b.Unload(0)
		close(unloadDone)
	}()

	for _, p := range []*fakeProcess{a, c} {
		select {
		case <-p.stopStarted:
		case <-time.After(2 * time.Second):
			t.Fatalf("Stop on %s never started — same-timeout unloads are not parallel", p.id)
		}
	}
	close(a.stopBlock)
	close(c.stopBlock)

	select {
	case <-unloadDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Unload did not return after stops were released")
	}
	if a.lastStopTimeout() != 25*time.Second || c.lastStopTimeout() != 25*time.Second {
		t.Errorf("stop timeouts a=%v c=%v want 25s each", a.lastStopTimeout(), c.lastStopTimeout())
	}
}

func TestBaseRouter_UnloadPositiveTimeoutOverridesConfigured(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()

	conf := config.Config{
		HealthCheckTimeout: 5,
		UnloadTimeout:      25,
		Models: map[string]config.ModelConfig{
			"a": {UnloadTimeout: 45},
		},
	}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a}, &stubPlanner{})
	b.Unload(time.Second, "a")

	if a.lastStopTimeout() != time.Second {
		t.Errorf("a stop timeout=%v want 1s", a.lastStopTimeout())
	}
}

// TestBaseRouter_Unload_StopsInParallel verifies that Unload fans out its
// Stop calls concurrently rather than stopping each process serially. Each
// fakeProcess.Stop is pinned via stopBlock; the test only releases them
// after observing every stopStarted, proving all three Stops were in
// flight simultaneously.
func TestBaseRouter_Unload_StopsInParallel(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	a.stopBlock = make(chan struct{})
	pb := newFakeProcess("b")
	pb.markReady()
	pb.stopBlock = make(chan struct{})
	pc := newFakeProcess("c")
	pc.markReady()
	pc.stopBlock = make(chan struct{})

	b := newTestBase(t, map[string]process.Process{"a": a, "b": pb, "c": pc}, &stubPlanner{})

	unloadDone := make(chan struct{})
	go func() {
		b.Unload(time.Second, "a", "b", "c")
		close(unloadDone)
	}()

	// All three Stop calls must start before any of them are allowed to
	// complete. If Unload was serial, only one stopStarted would fire
	// until we released its stopBlock, and this would deadlock.
	for _, p := range []*fakeProcess{a, pb, pc} {
		select {
		case <-p.stopStarted:
		case <-time.After(2 * time.Second):
			t.Fatalf("Stop on %s never started — Unload is not parallel", p.id)
		}
	}

	// Release them; Unload should now return.
	close(a.stopBlock)
	close(pb.stopBlock)
	close(pc.stopBlock)

	select {
	case <-unloadDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Unload did not return after stops released")
	}

	for _, p := range []*fakeProcess{a, pb, pc} {
		if p.State() != process.StateStopped {
			t.Errorf("%s state=%q want stopped", p.id, p.State())
		}
		if got := p.stopCalls.Load(); got != 1 {
			t.Errorf("%s stopCalls=%d want 1", p.id, got)
		}
	}
}

func TestBaseRouter_OnDemandStart(t *testing.T) {
	a := newFakeProcess("a")
	a.autoReady = true

	b := newTestBase(t, map[string]process.Process{"a": a}, &stubPlanner{})

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newRequest("a"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.runCalls.Load(); got != 1 {
		t.Errorf("runCalls=%d want 1", got)
	}
	if got := a.serveCalls.Load(); got != 1 {
		t.Errorf("serveCalls=%d want 1", got)
	}
}

func TestBaseRouter_IgnoreWebsocketsRejectsModelUnlessReady(t *testing.T) {
	for _, state := range []process.ProcessState{process.StateStopped, process.StateStarting} {
		t.Run(string(state), func(t *testing.T) {
			a := newFakeProcess("a")
			if state != process.StateStopped {
				a.setState(state)
			}
			conf := config.Config{
				HealthCheckTimeout: 5,
				Models: map[string]config.ModelConfig{
					"a": {Compat: config.CompatConfig{IgnoreWebsockets: true}},
				},
			}
			b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a}, &stubPlanner{})

			r := httptest.NewRequest(http.MethodGet, "/props?model=a", nil)
			r.Header.Set("Connection", "keep-alive, Upgrade")
			r.Header.Set("Upgrade", "websocket")
			w := httptest.NewRecorder()
			b.ServeHTTP(w, r)

			if w.Code != http.StatusConflict {
				t.Fatalf("status=%d want %d body=%q", w.Code, http.StatusConflict, w.Body.String())
			}
			if got := a.runCalls.Load(); got != 0 {
				t.Errorf("runCalls=%d want 0", got)
			}
			if got := a.serveCalls.Load(); got != 0 {
				t.Errorf("serveCalls=%d want 0", got)
			}
		})
	}
}

func TestBaseRouter_WebsocketStartsModelWhenCompatDisabled(t *testing.T) {
	a := newFakeProcess("a")
	a.autoReady = true
	conf := config.Config{
		HealthCheckTimeout: 5,
		Models:             map[string]config.ModelConfig{"a": {}},
	}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a}, &stubPlanner{})

	r := httptest.NewRequest(http.MethodGet, "/props?model=a", nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	b.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want %d body=%q", w.Code, http.StatusOK, w.Body.String())
	}
	if got := a.runCalls.Load(); got != 1 {
		t.Errorf("runCalls=%d want 1", got)
	}
}

func TestBaseRouter_IgnoreWebsocketsDoesNotBlockSwap(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	a.serveBlock = make(chan struct{})
	pb := newFakeProcess("b")
	pb.autoReady = true
	conf := config.Config{
		HealthCheckTimeout: 5,
		Models: map[string]config.ModelConfig{
			"a": {Compat: config.CompatConfig{IgnoreWebsockets: true}},
			"b": {},
		},
	}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a, "b": pb}, &stubPlanner{
		evict: map[string][]string{"b": {"a"}},
	})

	websocketDone := make(chan struct{})
	go func() {
		defer close(websocketDone)
		r := httptest.NewRequest(http.MethodGet, "/props?model=a", nil)
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		b.ServeHTTP(httptest.NewRecorder(), r)
	}()
	waitSignal(t, a.serveStarted, "websocket request start")

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newRequest("b"))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if !a.stoppedWhileServing.Load() {
		t.Fatal("ignored websocket prevented the conflicting model from swapping in")
	}

	close(a.serveBlock)
	waitSignal(t, websocketDone, "websocket request finish")
}

// TestBaseRouter_SwapUsesUnloadTimeoutForEvictions asserts a swap stops the
// evicted model with its configured unloadTimeout, not the (longer) cold-start
// healthCheckTimeout the incoming target uses. doSwap previously used
// healthCheckTimeout for both, silently ignoring the documented per-model
// unloadTimeout on the swap path.
func TestBaseRouter_SwapUsesUnloadTimeoutForEvictions(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	pb := newFakeProcess("b")
	pb.autoReady = true
	conf := config.Config{
		HealthCheckTimeout: 5,
		Models: map[string]config.ModelConfig{
			"a": {UnloadTimeout: 2},
			"b": {},
		},
	}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a, "b": pb}, &stubPlanner{
		evict: map[string][]string{"b": {"a"}},
	})

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newRequest("b"))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.lastStopTimeout(); got != 2*time.Second {
		t.Fatalf("evicted model stop timeout = %v, want 2s (unloadTimeout, not the 5s healthCheckTimeout)", got)
	}
}

// memGateConfig builds a two-model config with the memory-admission ledger on:
// pool 100, a and b at 60 each, so neither fits beside the other but either fits
// once the planner's eviction is credited. pool == 0 turns the ledger off with
// the same model set, which is the control for the hot path.
func memGateConfig(pool int64) config.Config {
	return config.Config{
		HealthCheckTimeout: 5,
		MemoryPool:         pool,
		Models: map[string]config.ModelConfig{
			"a": {MemoryCeiling: 60, UnloadTimeout: 1},
			"b": {MemoryCeiling: 60},
		},
	}
}

// TestBaseRouter_SwapAbortsWhenEvictionFailsUnderMemoryGate is the regression
// test for todo 1.6 (H2). With the ledger on, the fit check admitted b only
// because it credited a's ceiling as freed by the eviction. If a's Stop does not
// actually complete (a forced kill that leaves the upstream container alive),
// loading b anyway puts 120 on a 100 pool — both ceilings counted as available
// when only one was freed. The swap must fail instead of starting the target.
func TestBaseRouter_SwapAbortsWhenEvictionFailsUnderMemoryGate(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	a.stopErr = process.ErrForcedKill // stop reports failure and a stays resident
	pb := newFakeProcess("b")
	pb.autoReady = true

	b := newTestBaseWithConfig(t, memGateConfig(100), map[string]process.Process{"a": a, "b": pb}, &stubPlanner{
		evict: map[string][]string{"b": {"a"}},
	})

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newRequest("b"))

	if w.Code == http.StatusOK {
		t.Fatalf("swap succeeded despite a failed eviction: status=%d body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "eviction did not complete") {
		t.Errorf("error body does not name the failed eviction: %q", w.Body.String())
	}
	if got := a.stopCalls.Load(); got != 1 {
		t.Errorf("a.stopCalls=%d want 1 (the eviction must still have been attempted)", got)
	}
	// The target must never have been asked to start: EnsureReady is the only
	// path that loads it, and asking is already too late.
	select {
	case <-pb.ensureAsked:
		t.Fatal("EnsureReady called on the target after the eviction failed")
	default:
	}
	if got := pb.runCalls.Load(); got != 0 {
		t.Errorf("b.runCalls=%d want 0", got)
	}
	if got := pb.serveCalls.Load(); got != 0 {
		t.Errorf("b.serveCalls=%d want 0", got)
	}
}

// TestBaseRouter_SwapProceedsWhenEvictionFailsWithoutMemoryGate pins the hot
// path: with the ledger off (memoryPool == 0, every box that has not configured
// it) a failed evictee stop is still only logged and the swap loads the target,
// exactly as before todo 1.6.
func TestBaseRouter_SwapProceedsWhenEvictionFailsWithoutMemoryGate(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	a.stopErr = process.ErrForcedKill
	pb := newFakeProcess("b")
	pb.autoReady = true

	b := newTestBaseWithConfig(t, memGateConfig(0), map[string]process.Process{"a": a, "b": pb}, &stubPlanner{
		evict: map[string][]string{"b": {"a"}},
	})

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newRequest("b"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%q", w.Code, w.Body.String())
	}
	if got := pb.serveCalls.Load(); got != 1 {
		t.Errorf("b.serveCalls=%d want 1", got)
	}
}

// TestBaseRouter_SwapProceedsWhenEvictionSucceedsUnderMemoryGate is the
// happy-path half of todo 1.6: with the ledger on and the evictee stopping
// cleanly, the swap is unchanged.
func TestBaseRouter_SwapProceedsWhenEvictionSucceedsUnderMemoryGate(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	pb := newFakeProcess("b")
	pb.autoReady = true

	b := newTestBaseWithConfig(t, memGateConfig(100), map[string]process.Process{"a": a, "b": pb}, &stubPlanner{
		evict: map[string][]string{"b": {"a"}},
	})

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newRequest("b"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%q", w.Code, w.Body.String())
	}
	if got := a.State(); got != process.StateStopped {
		t.Errorf("a state=%q want stopped", got)
	}
	if got := pb.serveCalls.Load(); got != 1 {
		t.Errorf("b.serveCalls=%d want 1", got)
	}
}

// TestBaseRouter_RequestDuringStop is the router-level regression test for
// issue #946. A process being stopped outside the router's knowledge (a TTL
// unload, a crash, an operator kill) must not wedge the swap machinery: the
// request has to wait for the stop to finish and then start the model.
//
// Before the fix doSwap read State(), saw StateStopping, skipped the start, and
// then subscribed to a process nobody would ever start — stranding the swap, so
// every later request for the model joined the same zombie swap and hung.
func TestBaseRouter_RequestDuringStop(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	a.autoReady = true
	// Pin Stop so the process sits in StateStopping while the request arrives.
	a.stopBlock = make(chan struct{})

	b := newTestBase(t, map[string]process.Process{"a": a}, &stubPlanner{})

	// Stop the process directly, the way the process's own TTL goroutine does —
	// the router is never told about it.
	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		_ = a.Stop(time.Second)
	}()
	waitSignal(t, a.stopStarted, "a.stopStarted")

	if got := a.State(); got != process.StateStopping {
		t.Fatalf("State()=%s want %s before request", got, process.StateStopping)
	}

	w := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		b.ServeHTTP(w, newRequest("a"))
	}()

	// The router must ask the process to start even though it is mid-stop, and
	// leave the process to decide when. The stop is still pinned here, so this
	// signal can only arrive from a start requested during StateStopping —
	// which is precisely what the old State()-then-Run code refused to do.
	waitSignal(t, a.ensureAsked, "a.ensureAsked")

	// Let the unload complete. The request must now start the model itself.
	close(a.stopBlock)
	<-stopDone

	select {
	case <-served:
	case <-t.Context().Done():
		t.Fatalf("request during stop never completed: %v", context.Cause(t.Context()))
	}

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.runCalls.Load(); got != 1 {
		t.Errorf("runCalls=%d want 1 (model must be restarted after the unload)", got)
	}
	if got := a.serveCalls.Load(); got != 1 {
		t.Errorf("serveCalls=%d want 1", got)
	}
}

func TestBaseRouter_ContextCancel(t *testing.T) {
	a := newFakeProcess("a")
	// autoReady=false so swap parks forever until we mark ready.

	b := newTestBase(t, map[string]process.Process{"a": a}, &stubPlanner{})

	ctx, cancel := context.WithCancel(context.Background())
	w1 := httptest.NewRecorder()
	done1 := make(chan struct{})
	go func() {
		b.ServeHTTP(w1, newRequestCtx(ctx, "a"))
		close(done1)
	}()

	w2 := httptest.NewRecorder()
	done2 := make(chan struct{})
	go func() {
		b.ServeHTTP(w2, newRequest("a"))
		close(done2)
	}()

	waitProcessed(t, b.testProcessed, 2) // both requests joined the active swap
	<-a.runStarted

	cancel()
	select {
	case <-done1:
	case <-time.After(time.Second):
		t.Fatal("cancelled ServeHTTP did not return after ctx cancel")
	}

	a.markReady()
	select {
	case <-done2:
	case <-time.After(time.Second):
		t.Fatal("non-cancelled ServeHTTP did not complete after swap")
	}
	if w2.Code != http.StatusOK {
		t.Errorf("second request status=%d body=%q", w2.Code, w2.Body.String())
	}
}

func TestBaseRouter_ModelNotFound(t *testing.T) {
	a := newFakeProcess("a")
	b := newTestBase(t, map[string]process.Process{"a": a}, &stubPlanner{})

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newRequest("unknown"))

	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d want %d body=%q", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestBaseRouter_ConcurrencyLimitRejectsBeforeLoadingStream(t *testing.T) {
	sendLoading := true
	conf := config.Config{
		HealthCheckTimeout: 5,
		Models: map[string]config.ModelConfig{
			"a": {ConcurrencyLimit: 2, SendLoadingState: &sendLoading},
			"b": {},
		},
	}
	a := newFakeProcess("a")
	a.autoReady = true
	bProc := newFakeProcess("b")
	bProc.autoReady = true
	bProc.serveBlock = make(chan struct{})

	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a, "b": bProc}, &stubPlanner{
		evict: map[string][]string{"a": {"b"}},
	})

	bDone := make(chan struct{})
	go func() {
		b.ServeHTTP(httptest.NewRecorder(), newStreamRequest("b"))
		close(bDone)
	}()
	waitSignal(t, bProc.serveStarted, "b request start")
	waitProcessed(t, b.testProcessed, 2)

	aDone1 := make(chan struct{})
	aDone2 := make(chan struct{})
	go func() {
		b.ServeHTTP(httptest.NewRecorder(), newStreamRequest("a"))
		close(aDone1)
	}()
	go func() {
		b.ServeHTTP(httptest.NewRecorder(), newStreamRequest("a"))
		close(aDone2)
	}()
	waitProcessed(t, b.testProcessed, 2)

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newStreamRequest("a"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429 body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q want application/json", got)
	}
	if strings.Contains(w.Body.String(), "llama-swap loading model") {
		t.Fatalf("429 body contains loading stream: %q", w.Body.String())
	}
	// OpenAI clients read body["error"]["message"], so "error" must decode as
	// an object rather than a bare string.
	var envelope swaputil.ErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("429 body is not an OpenAI error envelope: %v, body=%q", err, w.Body.String())
	}
	if envelope.Error.Message == "" || envelope.Error.Type != swaputil.ErrorTypeRateLimit {
		t.Fatalf("429 error=%+v, want a rate_limit_error with a message", envelope.Error)
	}

	close(bProc.serveBlock)
	for name, ch := range map[string]chan struct{}{"b": bDone, "a1": aDone1, "a2": aDone2} {
		waitSignal(t, ch, name+" request finish")
	}
}

// TestBaseRouter_MemoryRefusalRejectsBeforeLoadingStream is the regression test
// for todo 1.7 (C1). A never-fits memory refusal is a decision the scheduler can
// make immediately, so it must be delivered on the admission channel — like the
// concurrency-limit rejection above — rather than after admission succeeded.
// Delivered late, a streaming client has already been handed 200 + SSE headers
// by the loading writer and the 503 can only be framed into the stream as an
// error frame, which is the failure-reported-as-success shape of #1029. The
// non-streaming path was always a clean 503; this brings streaming in line.
func TestBaseRouter_MemoryRefusalRejectsBeforeLoadingStream(t *testing.T) {
	sendLoading := true
	conf := config.Config{
		HealthCheckTimeout: 5,
		MemoryPool:         100,
		Models: map[string]config.ModelConfig{
			// ceiling 200 > pool 100: never fits, no eviction can help.
			"big": {MemoryCeiling: 200, SendLoadingState: &sendLoading},
		},
	}
	big := newFakeProcess("big")
	big.autoReady = true
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"big": big}, &stubPlanner{})

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newStreamRequest("big"))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503 body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q want application/json", got)
	}
	body := w.Body.String()
	if strings.Contains(body, "llama-swap loading model") || strings.Contains(body, "data: ") {
		t.Fatalf("503 body contains a partial SSE stream: %q", body)
	}
	var envelope swaputil.ErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("503 body is not an OpenAI error envelope: %v, body=%q", err, body)
	}
	if envelope.Error.Code != "memory_admission" || envelope.Error.Type != swaputil.ErrorTypeServer {
		t.Fatalf("503 error=%+v, want a server_error with code memory_admission", envelope.Error)
	}
	if got := big.runCalls.Load(); got != 0 {
		t.Errorf("big.runCalls=%d want 0 (a refused model must never be started)", got)
	}
}

// TestBaseRouter_MemoryRefusalIsCleanForNonStreaming pins the behaviour the
// streaming path above is being brought in line with.
func TestBaseRouter_MemoryRefusalIsCleanForNonStreaming(t *testing.T) {
	conf := config.Config{
		HealthCheckTimeout: 5,
		MemoryPool:         100,
		Models:             map[string]config.ModelConfig{"big": {MemoryCeiling: 200}},
	}
	big := newFakeProcess("big")
	big.autoReady = true
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"big": big}, &stubPlanner{})

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newRequest("big"))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503 body=%q", w.Code, w.Body.String())
	}
	if got := big.runCalls.Load(); got != 0 {
		t.Errorf("big.runCalls=%d want 0", got)
	}
}

// TestBaseRouter_DispatchErrorFramedIntoLoadingStream covers the second half of
// #1029. Once the loading stream has committed its 200, an error can only reach
// the client in-band: swaputil.SendError's status is dropped and its JSON body
// lands as a bare line that every SSE parser discards, leaving the caller with
// a truncated stream, no [DONE], and no reason.
func TestBaseRouter_DispatchErrorFramedIntoLoadingStream(t *testing.T) {
	sendLoading := true
	conf := config.Config{
		HealthCheckTimeout: 5,
		Models:             map[string]config.ModelConfig{"a": {SendLoadingState: &sendLoading}},
	}
	a := newFakeProcess("a")
	a.ensureErr = fmt.Errorf("upstream command exited prematurely")

	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a}, &stubPlanner{})

	w := httptest.NewRecorder()
	b.ServeHTTP(w, newStreamRequest("a"))

	body := w.Body.String()
	// The loading text is streamed a few characters per frame, so reassemble it.
	if content := extractStreamedContent(body); !strings.Contains(content, "llama-swap loading model") {
		t.Fatalf("loading stream did not start, so this is not the path under test: %q", content)
	}
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if line != "" && !strings.HasPrefix(line, "data: ") {
			t.Errorf("line %q is not an SSE field; a client would silently ignore it", line)
		}
	}
	if !strings.Contains(body, "upstream command exited prematurely") {
		t.Errorf("dispatch error never reached the client: %q", body)
	}
	if !strings.HasSuffix(strings.TrimRight(body, "\n"), "data: [DONE]") {
		t.Errorf("stream not terminated with [DONE]: %q", body)
	}
}

func TestBaseRouter_Shutdown_StopsAllProcesses(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	go a.Run(0)
	pb := newFakeProcess("b")
	pb.markReady()
	go pb.Run(0)

	b := newTestBase(t, map[string]process.Process{"a": a, "b": pb}, &stubPlanner{})

	if err := b.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := a.stopCalls.Load(); got != 1 {
		t.Errorf("a.stopCalls=%d want 1", got)
	}
	if got := pb.stopCalls.Load(); got != 1 {
		t.Errorf("b.stopCalls=%d want 1", got)
	}

	// Subsequent ServeHTTP should report 5xx.
	w := httptest.NewRecorder()
	b.ServeHTTP(w, newRequest("a"))
	if w.Code != http.StatusInternalServerError && w.Code != http.StatusServiceUnavailable {
		t.Errorf("post-shutdown status=%d want 5xx body=%q", w.Code, w.Body.String())
	}

	// Second Shutdown should report already in progress.
	if err := b.Shutdown(0); err == nil {
		t.Errorf("second Shutdown returned nil, want error")
	}
}

func TestBaseRouter_Shutdown_DetachesConfiguredModels(t *testing.T) {
	keep := newFakeProcess("keep")
	keep.markReady()
	go keep.Run(0)
	drop := newFakeProcess("drop")
	drop.markReady()
	go drop.Run(0)

	conf := config.Config{
		Models: map[string]config.ModelConfig{
			"keep": {DetachOnShutdown: true},
			"drop": {},
		},
	}
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"keep": keep, "drop": drop}, &stubPlanner{})

	if err := b.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// keep is detach-configured → Detach (CmdStop skipped, container preserved).
	if got := keep.detachCalls.Load(); got != 1 {
		t.Errorf("keep.detachCalls=%d want 1 (detach-configured model must be detached)", got)
	}
	// drop is not → Stop (normal teardown, CmdStop runs).
	if got := drop.detachCalls.Load(); got != 0 {
		t.Errorf("drop.detachCalls=%d want 0 (non-detach model must be stopped, not detached)", got)
	}
	if got := drop.stopCalls.Load(); got != 1 {
		t.Errorf("drop.stopCalls=%d want 1", got)
	}
}

// memBlockedConfig is the live GB10 shape behind P1, scaled to small units:
// pool 121 - reserve 10 = budget 111; parakeet-asr (8) + qwen-asr (14) stay
// resident beside the sglang model qwen38-27b (83); flash-vllm (100) evicts only
// qwen38-27b, and 100 + 22 > 111.
func memBlockedConfig() config.Config {
	sendLoading := true
	return config.Config{
		HealthCheckTimeout: 5,
		MemoryPool:         121,
		MemoryReserve:      10,
		Models: map[string]config.ModelConfig{
			"parakeet-asr": {MemoryCeiling: 8},
			"qwen-asr":     {MemoryCeiling: 14},
			"qwen38-27b":   {MemoryCeiling: 83},
			"flash-vllm":   {MemoryCeiling: 100, SendLoadingState: &sendLoading},
		},
	}
}

// TestBaseRouter_MemoryBlockedRefusedImmediately is the router-level P1
// regression: a load that does not fit beside residents the planner will not
// evict, with nothing in flight that could free memory, used to queue forever
// (a streaming client got an endless "Queue position" stream). It must now be a
// clean, immediate 503 on both the streaming and non-streaming paths.
func TestBaseRouter_MemoryBlockedRefusedImmediately(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  func(string) *http.Request
	}{
		{"non-streaming", newRequest},
		{"streaming", newStreamRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			procs := map[string]process.Process{}
			for _, id := range []string{"parakeet-asr", "qwen-asr", "qwen38-27b", "flash-vllm"} {
				p := newFakeProcess(id)
				p.autoReady = true
				if id != "flash-vllm" {
					p.markReady()
				}
				procs[id] = p
			}
			b := newTestBaseWithConfig(t, memBlockedConfig(), procs, &stubPlanner{
				evict: map[string][]string{"flash-vllm": {"qwen38-27b"}},
			})

			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				b.ServeHTTP(w, tc.req("flash-vllm"))
				close(done)
			}()
			waitSignal(t, done, "flash-vllm request")

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d want 503 body=%q", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if strings.Contains(body, "data: ") {
				t.Fatalf("503 body contains a partial SSE stream: %q", body)
			}
			var envelope swaputil.ErrorEnvelope
			if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("503 body is not an OpenAI error envelope: %v, body=%q", err, body)
			}
			if envelope.Error.Code != "memory_admission" {
				t.Fatalf("error=%+v want code memory_admission", envelope.Error)
			}
			for _, want := range []string{"parakeet-asr=8", "qwen-asr=14"} {
				if !strings.Contains(envelope.Error.Message, want) {
					t.Errorf("message %q does not name blocker %q", envelope.Error.Message, want)
				}
			}
			if got := procs["qwen38-27b"].(*fakeProcess).stopCalls.Load(); got != 0 {
				t.Errorf("qwen38-27b.stopCalls=%d want 0 (a refused load must not evict anything)", got)
			}
			if got := procs["flash-vllm"].(*fakeProcess).runCalls.Load(); got != 0 {
				t.Errorf("flash-vllm.runCalls=%d want 0", got)
			}
		})
	}
}

// TestBaseRouter_MemoryDrainRefusalFramedIntoLoadingStream: a streaming load
// that queued while a swap was evicting, and still does not fit once that swap
// lands, is refused at drain time. Its loading stream has already committed a
// 200, so the refusal must arrive in-band as an SSE error event + [DONE] that
// names why, not as a bare JSON line an SSE parser would drop.
func TestBaseRouter_MemoryDrainRefusalFramedIntoLoadingStream(t *testing.T) {
	sendLoading := true
	conf := config.Config{
		HealthCheckTimeout: 5,
		MemoryPool:         100,
		Models: map[string]config.ModelConfig{
			"a": {MemoryCeiling: 60, UnloadTimeout: 1},
			"b": {MemoryCeiling: 30},
			"c": {MemoryCeiling: 80, SendLoadingState: &sendLoading},
		},
	}
	a := newFakeProcess("a")
	a.markReady()
	a.stopBlock = make(chan struct{}) // hold b's eviction of a in flight
	pb := newFakeProcess("b")
	pb.autoReady = true
	pc := newFakeProcess("c")
	pc.autoReady = true
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a, "b": pb, "c": pc}, &stubPlanner{
		evict: map[string][]string{"b": {"a"}},
	})

	wb := httptest.NewRecorder()
	bDone := make(chan struct{})
	go func() {
		b.ServeHTTP(wb, newRequest("b"))
		close(bDone)
	}()
	waitProcessed(t, b.testProcessed, 1)
	waitSignal(t, a.stopStarted, "eviction of a")

	wc := httptest.NewRecorder()
	cDone := make(chan struct{})
	go func() {
		b.ServeHTTP(wc, newStreamRequest("c"))
		close(cDone)
	}()
	waitProcessed(t, b.testProcessed, 1) // c admitted and queued behind b's swap

	close(a.stopBlock)
	waitSignal(t, bDone, "b request")
	waitSignal(t, cDone, "c request")

	if wb.Code != http.StatusOK {
		t.Fatalf("b status=%d want 200 body=%q", wb.Code, wb.Body.String())
	}
	body := wc.Body.String()
	if content := extractStreamedContent(body); !strings.Contains(content, "llama-swap loading model") {
		t.Fatalf("loading stream did not start, so this is not the path under test: %q", body)
	}
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if line != "" && !strings.HasPrefix(line, "data: ") {
			t.Errorf("line %q is not an SSE field; a client would silently ignore it", line)
		}
	}
	for _, want := range []string{`\"c\" needs 80 bytes`, "b=30"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q: %q", want, body)
		}
	}
	// The frame keeps the refusal's own 503 envelope code; a generic 500
	// internal_error would tell the client nothing retryable happened.
	if !strings.Contains(body, `"code":"memory_admission"`) || strings.Contains(body, "internal_error") {
		t.Errorf("error frame lost the memory_admission code: %q", body)
	}
	if !strings.HasSuffix(strings.TrimRight(body, "\n"), "data: [DONE]") {
		t.Errorf("stream not terminated with [DONE]: %q", body)
	}
	if got := pc.runCalls.Load(); got != 0 {
		t.Errorf("c.runCalls=%d want 0", got)
	}
}

// TestBaseRouter_RetryAfterForcedKillWaitsForLeak is the router-level P2
// regression. b's swap force-kills its evictee a: a real process then reads
// Stopped (the fake's forcedKill mode), while a's upstream is still answering.
// The first request fails (the todo 1.6 guard); the retry used to credit a as
// freed and load b on top. It must now wait until a's upstream stops answering
// on its proxy URL, then be served.
func TestBaseRouter_RetryAfterForcedKillWaitsForLeak(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // still up, even if unhealthy
	}))
	defer upstream.Close()

	conf := memGateConfig(100)
	ma := conf.Models["a"]
	ma.Proxy = upstream.URL
	ma.CheckEndpoint = "/health"
	conf.Models["a"] = ma

	a := newFakeProcess("a")
	a.markReady()
	a.forcedKill = true
	pb := newFakeProcess("b")
	pb.autoReady = true
	b := newTestBaseWithConfig(t, conf, map[string]process.Process{"a": a, "b": pb}, &stubPlanner{
		evict: map[string][]string{"b": {"a"}},
	})
	b.leakProbeInterval = 10 * time.Millisecond

	w1 := httptest.NewRecorder()
	b.ServeHTTP(w1, newRequest("b"))
	if w1.Code == http.StatusOK || !strings.Contains(w1.Body.String(), "eviction did not complete") {
		t.Fatalf("first request status=%d body=%q, want the failed-eviction error", w1.Code, w1.Body.String())
	}
	if got := a.State(); got != process.StateStopped {
		t.Fatalf("a state=%q want stopped (the real process's forced-kill end state)", got)
	}

	w2 := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		b.ServeHTTP(w2, newRequest("b"))
		close(done)
	}()
	select {
	case <-done:
		t.Fatalf("retry finished while a's upstream still answers: status=%d body=%q", w2.Code, w2.Body.String())
	case <-time.After(200 * time.Millisecond): // many probe intervals
	}
	if got := pb.runCalls.Load(); got != 0 {
		t.Fatalf("b.runCalls=%d want 0 while a's leak is live", got)
	}

	upstream.Close() // a's container finally goes away
	waitSignal(t, done, "retry after the leak cleared")
	if w2.Code != http.StatusOK {
		t.Fatalf("retry status=%d want 200 body=%q", w2.Code, w2.Body.String())
	}
	if got := pb.serveCalls.Load(); got != 1 {
		t.Errorf("b.serveCalls=%d want 1", got)
	}
}

// TestBaseRouter_UpstreamGone pins the leak probe's clear condition: only a
// connection-level failure means the upstream is gone. Any HTTP answer, even a
// 5xx, means a server is still up (and so may still hold memory), and a timeout
// cannot tell gone from hung.
func TestBaseRouter_UpstreamGone(t *testing.T) {
	client := &http.Client{Timeout: 100 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
	ctx := context.Background()

	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer errSrv.Close()
	if upstreamGone(ctx, client, errSrv.URL+"/health") {
		t.Error("5xx answer treated as gone")
	}

	release := make(chan struct{})
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer slowSrv.Close()
	defer close(release)
	if upstreamGone(ctx, client, slowSrv.URL+"/health") {
		t.Error("timed-out probe treated as gone")
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL + "/health"
	closed.Close()
	if !upstreamGone(ctx, client, url) {
		t.Error("refused connection not treated as gone")
	}
}

func TestBaseRouter_LeakProbeURL(t *testing.T) {
	for _, tc := range []struct {
		mc   config.ModelConfig
		want string
	}{
		{config.ModelConfig{Proxy: "http://127.0.0.1:8990", CheckEndpoint: "/health"}, "http://127.0.0.1:8990/health"},
		{config.ModelConfig{Proxy: "http://127.0.0.1:8990/", CheckEndpoint: "none"}, "http://127.0.0.1:8990/"},
		{config.ModelConfig{Proxy: "http://127.0.0.1:8990"}, "http://127.0.0.1:8990/"},
		{config.ModelConfig{}, ""},
	} {
		if got := leakProbeURL(tc.mc); got != tc.want {
			t.Errorf("leakProbeURL(%+v)=%q want %q", tc.mc, got, tc.want)
		}
	}
}

// TestBaseRouter_StopProcessesReportsForcedKills: Unload's stop path reports
// which stops were forced so the scheduler can keep charging them.
func TestBaseRouter_StopProcessesReportsForcedKills(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	a.forcedKill = true
	c := newFakeProcess("c")
	c.markReady()
	b := newTestBase(t, map[string]process.Process{"a": a, "c": c}, &stubPlanner{})
	forced := b.StopProcesses(time.Second, []string{"a", "c", "unknown"})
	if len(forced) != 1 || forced[0] != "a" {
		t.Fatalf("forced=%v want [a]", forced)
	}
}

// TestBaseRouter_UnloadDuringEvictionDoesNotStartTarget: the UI's "Cancel load"
// is an unload of the loading model. When it lands while the swap is still
// stopping its evictee, the swap used to carry on and boot the target after
// the unload had returned. The swap's ctx is now cancelled, so doSwap reports
// an error instead of starting it, and a later request for the model loads it
// normally.
func TestBaseRouter_UnloadDuringEvictionDoesNotStartTarget(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	a.stopBlock = make(chan struct{})
	pb := newFakeProcess("b")
	pb.autoReady = true
	b := newTestBaseWithConfig(t, memGateConfig(100), map[string]process.Process{"a": a, "b": pb}, &stubPlanner{
		evict: map[string][]string{"b": {"a"}},
	})

	wb := httptest.NewRecorder()
	bDone := make(chan struct{})
	go func() {
		b.ServeHTTP(wb, newRequest("b"))
		close(bDone)
	}()
	waitProcessed(t, b.testProcessed, 1)
	waitSignal(t, a.stopStarted, "eviction of a")

	b.Unload(time.Second, "b")
	waitProcessed(t, b.testProcessed, 1)
	waitSignal(t, bDone, "b request released by the unload")
	if wb.Code == http.StatusOK || !strings.Contains(wb.Body.String(), "model unloaded") {
		t.Fatalf("b status=%d body=%q want the unload error", wb.Code, wb.Body.String())
	}

	close(a.stopBlock)                   // eviction finishes after the unload returned
	waitProcessed(t, b.testProcessed, 1) // the cancelled swap's SwapDone
	if got := pb.runCalls.Load(); got != 0 {
		t.Fatalf("b.runCalls=%d want 0: the cancelled swap must not start its target", got)
	}
	if got := pb.State(); got != process.StateStopped {
		t.Fatalf("b state=%q want stopped", got)
	}

	w2 := httptest.NewRecorder()
	b.ServeHTTP(w2, newRequest("b"))
	if w2.Code != http.StatusOK || pb.runCalls.Load() != 1 {
		t.Fatalf("later request status=%d runCalls=%d body=%q want 200/1", w2.Code, pb.runCalls.Load(), w2.Body.String())
	}
}

// selfStopConfig: a (60) and c (60) cannot both fit a 100 pool, and the planner
// does not evict a for c, so c can only load once a has gone on its own.
func selfStopConfig(aProxy string) config.Config {
	return config.Config{
		HealthCheckTimeout: 5,
		MemoryPool:         100,
		Models: map[string]config.ModelConfig{
			"a": {MemoryCeiling: 60, Proxy: aProxy, CheckEndpoint: "/health"},
			"c": {MemoryCeiling: 60},
		},
	}
}

// TestBaseRouter_TTLStopDrainsQueuedLoad: c queued while a was in its TTL
// unload (StateStopping counts as pending). The TTL stop is not the router's,
// so it used to finish without any event and c was stranded until something
// unrelated drained the queue. The process's self-stop report now drains it.
func TestBaseRouter_TTLStopDrainsQueuedLoad(t *testing.T) {
	a := newFakeProcess("a")
	a.setState(process.StateStopping) // TTL unload in progress
	pc := newFakeProcess("c")
	pc.autoReady = true
	b := newTestBaseWithConfig(t, selfStopConfig(""), map[string]process.Process{"a": a, "c": pc}, &stubPlanner{})

	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		b.ServeHTTP(w, newRequest("c"))
		close(done)
	}()
	waitProcessed(t, b.testProcessed, 1)
	if got := pc.runCalls.Load(); got != 0 {
		t.Fatalf("c.runCalls=%d want 0 while a is stopping", got)
	}

	a.selfStop(nil) // the TTL stop finished
	waitSignal(t, done, "c request after a's TTL stop")
	if w.Code != http.StatusOK || pc.serveCalls.Load() != 1 {
		t.Fatalf("c status=%d serveCalls=%d body=%q want 200/1", w.Code, pc.serveCalls.Load(), w.Body.String())
	}
}

// TestBaseRouter_TTLForcedKillRecordsLeak: a TTL stop that had to force-kill
// used to be credited like a clean stop, loading c on top of a container that
// may still hold a's memory. It is now a leak: c waits until a's upstream stops
// answering, then loads.
func TestBaseRouter_TTLForcedKillRecordsLeak(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	a := newFakeProcess("a")
	a.markReady() // a TTL stop starts from Ready, so a was seen healthy
	a.setState(process.StateStopping)
	pc := newFakeProcess("c")
	pc.autoReady = true
	b := newTestBaseWithConfig(t, selfStopConfig(upstream.URL), map[string]process.Process{"a": a, "c": pc}, &stubPlanner{})
	b.leakProbeInterval = 10 * time.Millisecond

	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		b.ServeHTTP(w, newRequest("c"))
		close(done)
	}()
	waitProcessed(t, b.testProcessed, 1)

	a.selfStop(process.ErrForcedKill)
	waitProcessed(t, b.testProcessed, 1) // the self-stop report
	select {
	case <-done:
		t.Fatalf("c finished while a's force-killed upstream still answers: status=%d body=%q", w.Code, w.Body.String())
	case <-time.After(200 * time.Millisecond): // many probe intervals
	}
	if got := pc.runCalls.Load(); got != 0 {
		t.Fatalf("c.runCalls=%d want 0 while a's leak is live", got)
	}

	upstream.Close()
	waitSignal(t, done, "c request after a's leak cleared")
	if w.Code != http.StatusOK {
		t.Fatalf("c status=%d want 200 body=%q", w.Code, w.Body.String())
	}
}

// leakCheckConfig: a (60) and c (60) cannot both fit a 100 pool, and the
// planner does not evict a for c. a's proxy refuses connections, so an HTTP
// probe of a would call it gone at once: only a's runningCheck, or an unload
// whose cmdStop completes, may release a leak on it.
func leakCheckConfig(runningCheck, cmdStop string) config.Config {
	closed := httptest.NewServer(http.NotFoundHandler())
	refused := closed.URL
	closed.Close()
	return config.Config{
		HealthCheckTimeout: 5,
		MemoryPool:         100,
		Models: map[string]config.ModelConfig{
			"a": {MemoryCeiling: 60, Proxy: refused, CheckEndpoint: "/health", RunningCheck: runningCheck, CmdStop: cmdStop},
			"c": {MemoryCeiling: 60},
		},
	}
}

func skipWithoutShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses sh -c commands")
	}
}

// leakA force-kills a through an unload so the scheduler records its leak.
func leakA(t *testing.T, b *baseRouter, a *fakeProcess) {
	t.Helper()
	a.forcedKill = true
	b.Unload(time.Second, "a")
	if got := a.State(); got != process.StateStopped {
		t.Fatalf("a state=%q want stopped", got)
	}
}

// serveAsync runs one request for model and closes done when it returns.
func serveAsync(b *baseRouter, r *http.Request) (*httptest.ResponseRecorder, chan struct{}) {
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		b.ServeHTTP(w, r)
		close(done)
	}()
	return w, done
}

// TestBaseRouter_RunningCheckDecidesLeak: with runningCheck set, the watcher
// runs it instead of probing the proxy. Exit 0 keeps the leak (c queues, even
// though a's port refuses and a was seen healthy, so the HTTP probe would have
// cleared it); the first non-zero exit clears it and c loads.
func TestBaseRouter_RunningCheckDecidesLeak(t *testing.T) {
	skipWithoutShell(t)
	marker := filepath.Join(t.TempDir(), "running")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	a := newFakeProcess("a")
	a.markReady()
	pc := newFakeProcess("c")
	pc.autoReady = true
	b := newTestBaseWithConfig(t, leakCheckConfig(fmt.Sprintf("sh -c 'test -e %s'", marker), ""),
		map[string]process.Process{"a": a, "c": pc}, &stubPlanner{})
	b.leakProbeInterval = 10 * time.Millisecond
	leakA(t, b, a)
	waitProcessed(t, b.testProcessed, 1)

	w, done := serveAsync(b, newRequest("c"))
	select {
	case <-done:
		t.Fatalf("c finished while a's runningCheck exits 0: status=%d body=%q", w.Code, w.Body.String())
	case <-time.After(300 * time.Millisecond): // many check intervals
	}
	if got := pc.runCalls.Load(); got != 0 {
		t.Fatalf("c.runCalls=%d want 0 while a's leak is live", got)
	}

	if err := os.Remove(marker); err != nil { // a's container goes away
		t.Fatal(err)
	}
	waitSignal(t, done, "c request after a's runningCheck exited non-zero")
	if w.Code != http.StatusOK || pc.serveCalls.Load() != 1 {
		t.Fatalf("c status=%d serveCalls=%d body=%q want 200/1", w.Code, pc.serveCalls.Load(), w.Body.String())
	}
}

// TestBaseRouter_RunningCheckTimeoutKeepsLeak: a runningCheck that outlives
// its timeout counts as "still running", and its whole process group is
// killed, so the command never gets to finish (the marker is never written).
func TestBaseRouter_RunningCheckTimeoutKeepsLeak(t *testing.T) {
	skipWithoutShell(t)
	marker := filepath.Join(t.TempDir(), "finished")
	a := newFakeProcess("a")
	a.markReady()
	pc := newFakeProcess("c")
	pc.autoReady = true
	b := newTestBaseWithConfig(t, leakCheckConfig(fmt.Sprintf("sh -c 'sleep 0.3 && touch %s && exit 1'", marker), ""),
		map[string]process.Process{"a": a, "c": pc}, &stubPlanner{})
	b.leakProbeInterval = 10 * time.Millisecond
	b.runningCheckTimeout = 50 * time.Millisecond
	leakA(t, b, a)
	waitProcessed(t, b.testProcessed, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, done := serveAsync(b, newRequestCtx(ctx, "c"))
	select {
	case <-done:
		t.Fatalf("c finished while a's runningCheck only timed out: status=%d body=%q", w.Code, w.Body.String())
	case <-time.After(700 * time.Millisecond): // past the check's own 0.3s sleep
	}
	if got := pc.runCalls.Load(); got != 0 {
		t.Fatalf("c.runCalls=%d want 0 while a's leak is live", got)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("timed-out runningCheck kept running (marker written): its process group was not killed")
	}
	cancel()
	waitSignal(t, done, "cancelled c request")
}

// TestBaseRouter_RunningCheckSignalDeathKeepsLeak: only a normal non-zero exit
// means "gone". A check killed by a signal (ExitCode -1) never answered, so it
// must count as still running, like a timeout.
func TestBaseRouter_RunningCheckSignalDeathKeepsLeak(t *testing.T) {
	skipWithoutShell(t)
	logger := logmon.NewWriter(io.Discard)
	check := func(cmd string) bool {
		t.Helper()
		args, err := config.SanitizeCommand(cmd)
		if err != nil {
			t.Fatalf("SanitizeCommand(%q): %v", cmd, err)
		}
		gone, _ := runningCheckGone(context.Background(), logger, "test", "a", args, os.Environ(), io.Discard, 5*time.Second)
		return gone
	}
	if check(`sh -c 'kill -KILL $$'`) {
		t.Error("runningCheck killed by SIGKILL treated as gone")
	}
	if check("sh -c 'exit 0'") {
		t.Error("runningCheck exiting 0 treated as gone")
	}
	if !check("sh -c 'exit 1'") {
		t.Error("runningCheck exiting 1 not treated as gone")
	}
}

// TestBaseRouter_NeverHealthyLeakNeedsUnload: a force-killed while still
// loading never answered its checkEndpoint, so its refused port proves
// nothing and, without runningCheck, nothing watches it. c is refused at once
// (not queued) with a 503 naming a, and a is released only by an unload whose
// cmdStop completes: a failing cmdStop keeps it counted.
func TestBaseRouter_NeverHealthyLeakNeedsUnload(t *testing.T) {
	skipWithoutShell(t)
	okFile := filepath.Join(t.TempDir(), "stopped")
	a := newFakeProcess("a")
	a.setState(process.StateStarting) // loading weights, port not open yet
	pc := newFakeProcess("c")
	pc.autoReady = true
	b := newTestBaseWithConfig(t, leakCheckConfig("", fmt.Sprintf("sh -c 'test -e %s'", okFile)),
		map[string]process.Process{"a": a, "c": pc}, &stubPlanner{})
	b.leakProbeInterval = 10 * time.Millisecond
	leakA(t, b, a)
	waitProcessed(t, b.testProcessed, 1)
	time.Sleep(100 * time.Millisecond) // many probe intervals: a must stay leaked

	refused := func(model string) {
		t.Helper()
		w, done := serveAsync(b, newRequest(model))
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s request did not return: queued behind a leak nothing will clear", model)
		}
		body := w.Body.String()
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(body, "memory_admission") ||
			!strings.Contains(body, "leaked: force-killed, still running?") || !strings.Contains(body, "unload a to release") {
			t.Fatalf("%s status=%d body=%q want a 503 naming leaked a", model, w.Code, body)
		}
	}
	refused("c")
	refused("a")

	b.Unload(time.Second, "a") // cmdStop exits 1: not released
	waitProcessed(t, b.testProcessed, 1)
	refused("c")

	if err := os.WriteFile(okFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	b.Unload(time.Second, "a") // cmdStop exits 0: released
	waitProcessed(t, b.testProcessed, 1)
	w := httptest.NewRecorder()
	b.ServeHTTP(w, newRequest("c"))
	if w.Code != http.StatusOK || pc.serveCalls.Load() != 1 {
		t.Fatalf("c status=%d serveCalls=%d body=%q want 200/1 after a was unloaded", w.Code, pc.serveCalls.Load(), w.Body.String())
	}
}
