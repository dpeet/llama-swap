package scheduler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// FIFO methods all run on the router's single run-loop goroutine, so these
// tests drive them directly and synchronously. A swap is "completed" by calling
// OnSwapDone, a served request "finishes" by calling OnServeDone — exactly the
// events the run loop would deliver. fakeEffects records every side-effect and
// stubPlanner supplies a fixed eviction set per target.

// stubPlanner returns a fixed eviction list per target.
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

// grantRec is one GrantError / GrantServe call. err!=nil marks an error grant;
// otherwise it is a serve grant and serve reports whether the caller received it.
type grantRec struct {
	model string
	err   error
	serve bool
}

type startRec struct {
	ctx   context.Context
	model string
	evict []string
}

type stopRec struct {
	timeout time.Duration
	ids     []string
}

// fakeEffects is an in-memory scheduler.Effects. Tests program process states
// and GrantServe outcomes, then assert on the recorded calls.
type fakeEffects struct {
	states       map[string]process.ProcessState // model -> state; missing => not handled
	serveResult  map[string]bool                 // GrantServe return per model (default true)
	lastServeReq HandlerReq

	starts []startRec
	grants []grantRec
	stops  []stopRec

	// forced marks models whose StopProcesses stop "force-killed": they are
	// reported back as forced (and left Stopped, like a real process).
	forced map[string]bool
	// watching is the set of models with a live leak watch.
	watching map[string]bool
	// unwatchable marks models WatchLeak cannot watch (no runningCheck and
	// never seen healthy): it returns false for them and starts nothing.
	unwatchable map[string]bool
	// rerunOK marks models whose RerunStop cmdStop completes; reruns records
	// every model RerunStop was asked to run.
	rerunOK map[string]bool
	reruns  []string
}

func newFakeEffects() *fakeEffects {
	return &fakeEffects{
		states:      map[string]process.ProcessState{},
		serveResult: map[string]bool{},
		forced:      map[string]bool{},
		watching:    map[string]bool{},
		unwatchable: map[string]bool{},
		rerunOK:     map[string]bool{},
	}
}

func (f *fakeEffects) ModelState(modelID string) (process.ProcessState, bool) {
	st, ok := f.states[modelID]
	return st, ok
}

func (f *fakeEffects) RunningModels() map[string]process.ProcessState {
	out := make(map[string]process.ProcessState)
	for id, st := range f.states {
		if st == process.StateStopped || st == process.StateShutdown {
			continue
		}
		out[id] = st
	}
	return out
}

func (f *fakeEffects) StartSwap(ctx context.Context, modelID string, evict []string) {
	f.starts = append(f.starts, startRec{ctx: ctx, model: modelID, evict: evict})
}

func (f *fakeEffects) GrantError(req HandlerReq, err error) {
	f.grants = append(f.grants, grantRec{model: req.Model, err: err})
}

func (f *fakeEffects) GrantServe(req HandlerReq, modelID string) bool {
	ok := true
	if v, set := f.serveResult[modelID]; set {
		ok = v
	}
	f.lastServeReq = req
	f.grants = append(f.grants, grantRec{model: modelID, serve: ok})
	return ok
}

func (f *fakeEffects) StopProcesses(timeout time.Duration, ids []string) []string {
	f.stops = append(f.stops, stopRec{timeout: timeout, ids: ids})
	var forced []string
	for _, id := range ids {
		if _, ok := f.states[id]; ok {
			f.states[id] = process.StateStopped
		}
		if f.forced[id] {
			forced = append(forced, id)
		}
	}
	return forced
}

func (f *fakeEffects) WatchLeak(modelID string) bool {
	if f.unwatchable[modelID] {
		return false
	}
	f.watching[modelID] = true
	return true
}

func (f *fakeEffects) RerunStop(_ time.Duration, ids []string) []string {
	var done []string
	for _, id := range ids {
		f.reruns = append(f.reruns, id)
		if f.rerunOK[id] {
			done = append(done, id)
		}
	}
	return done
}

func (f *fakeEffects) UnwatchLeak(modelID string) { delete(f.watching, modelID) }

// served counts grants that handed modelID a handler and were received.
func (f *fakeEffects) served(modelID string) int {
	n := 0
	for _, g := range f.grants {
		if g.err == nil && g.serve && g.model == modelID {
			n++
		}
	}
	return n
}

// errored counts error grants, optionally filtered by model ("" = any).
func (f *fakeEffects) errored(model string) int {
	n := 0
	for _, g := range f.grants {
		if g.err != nil && (model == "" || g.model == model) {
			n++
		}
	}
	return n
}

// startsFor counts StartSwap calls for modelID.
func (f *fakeEffects) startsFor(modelID string) int {
	n := 0
	for _, s := range f.starts {
		if s.model == modelID {
			n++
		}
	}
	return n
}

func newFIFO(planner Swapper, eff Effects) *FIFO {
	return NewFIFO("test", logmon.NewWriter(io.Discard), planner, config.FifoConfig{}, nil, 0, 0, eff)
}

func req(model string) HandlerReq {
	return HandlerReq{
		Model: model,
		Ctx:   context.Background(),
		Admit: make(chan error, 1),
	}
}

// reqCh creates a HandlerReq with a unique Respond channel so OnCancel can
// identify it among queued requests and swap waiters.
func reqCh(model string) HandlerReq {
	r := req(model)
	r.Respond = make(chan HandlerResp, 1)
	return r
}

func admitErr(t *testing.T, req HandlerReq) error {
	t.Helper()
	select {
	case err := <-req.Admit:
		return err
	default:
		t.Fatal("admission result not sent")
		return nil
	}
}

func assertAdmitted(t *testing.T, req HandlerReq) {
	t.Helper()
	if err := admitErr(t, req); err != nil {
		t.Fatalf("admission err=%v want nil", err)
	}
}

// assertAdmission503 asserts the request was refused on the ADMISSION channel
// with a MemoryAdmissionError. The channel matters as much as the error: it is
// the only seam before a streaming caller commits its 200 + SSE headers.
func assertAdmission503(t *testing.T, req HandlerReq) {
	t.Helper()
	err := admitErr(t, req)
	var memErr swaputil.MemoryAdmissionError
	if !errors.As(err, &memErr) {
		t.Fatalf("admission err=%v want MemoryAdmissionError", err)
	}
	if memErr.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("StatusCode()=%d want 503", memErr.StatusCode())
	}
}

func assertAdmission429(t *testing.T, req HandlerReq) {
	t.Helper()
	var httpErr swaputil.HTTPError
	err := admitErr(t, req)
	if !errors.As(err, &httpErr) {
		t.Fatalf("admission err=%v want HTTPError", err)
	}
	if httpErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("StatusCode()=%d want 429", httpErr.StatusCode())
	}
	if httpErr.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
}

func TestFIFO_SendAdmission_CancelledContextWins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := HandlerReq{
		Ctx:   ctx,
		Admit: make(chan error, 1),
	}

	if sendAdmission(r, nil) {
		t.Fatal("sendAdmission returned true for cancelled request")
	}
	select {
	case err := <-r.Admit:
		t.Fatalf("admission sent after cancellation: %v", err)
	default:
	}
}

func TestFIFO_SendAdmission_NilAdmitPreservesExistingBehavior(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if !sendAdmission(HandlerReq{Ctx: ctx}, nil) {
		t.Fatal("nil Admit should preserve existing accepted behavior")
	}
}

func TestFIFO_ReleaseWithoutReservationPanics(t *testing.T) {
	s := newFIFO(&stubPlanner{}, newFakeEffects())

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("release without reservation did not panic")
		}
	}()
	s.release("a")
}

func TestFIFO_FastPath(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	s := newFIFO(&stubPlanner{}, eff)

	s.OnRequest(req("a"))

	if got := eff.startsFor("a"); got != 0 {
		t.Errorf("StartSwap calls=%d want 0 (fast path should not swap)", got)
	}
	if got := eff.served("a"); got != 1 {
		t.Errorf("served(a)=%d want 1", got)
	}
}

func TestFIFO_GrantSetsPriorityMetadata(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	cfg := config.FifoConfig{Priority: map[string]int{"a": 7}}
	s := NewFIFO("test", logmon.NewWriter(io.Discard), &stubPlanner{}, cfg, nil, 0, 0, eff)

	ctx := swaputil.SetContext(context.Background(), swaputil.ReqContextData{ModelID: "a", Metadata: make(map[string]string)})
	s.OnRequest(HandlerReq{Model: "a", Ctx: ctx})

	if got := eff.served("a"); got != 1 {
		t.Fatalf("served(a)=%d want 1", got)
	}
	data, ok := swaputil.ReadContext(eff.lastServeReq.Ctx)
	if !ok {
		t.Fatal("context data missing from granted request")
	}
	if data.Metadata["fifo_priority"] != "7" {
		t.Errorf("fifo_priority = %q, want 7", data.Metadata["fifo_priority"])
	}
}

func TestFIFO_ModelNotFound(t *testing.T) {
	eff := newFakeEffects() // no states => model unknown
	s := newFIFO(&stubPlanner{}, eff)

	r := req("ghost")
	s.OnRequest(r)

	if got := len(eff.starts); got != 0 {
		t.Errorf("StartSwap calls=%d want 0", got)
	}
	if got := eff.errored("ghost"); got != 0 {
		t.Fatalf("error grants=%d want 0 for admission rejection", got)
	}
	if err := admitErr(t, r); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("admission err=%v want ErrModelNotFound", err)
	}
}

func TestFIFO_OnDemandStartThenServe(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateStopped
	s := newFIFO(&stubPlanner{}, eff)

	s.OnRequest(req("a"))
	if got := eff.startsFor("a"); got != 1 {
		t.Fatalf("StartSwap(a)=%d want 1", got)
	}
	if got := eff.served("a"); got != 0 {
		t.Errorf("served(a)=%d want 0 before swap completes", got)
	}

	// Swap finishes, model is now ready.
	eff.states["a"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "a"})

	if got := eff.served("a"); got != 1 {
		t.Errorf("served(a)=%d want 1 after swap done", got)
	}
}

func TestFIFO_JoinInFlightSwap(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateStopped
	s := newFIFO(&stubPlanner{}, eff)

	s.OnRequest(req("a")) // starts swap
	s.OnRequest(req("a")) // joins
	s.OnRequest(req("a")) // joins

	if got := eff.startsFor("a"); got != 1 {
		t.Fatalf("StartSwap(a)=%d want 1 (all three share one swap)", got)
	}

	eff.states["a"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "a"})

	if got := eff.served("a"); got != 3 {
		t.Errorf("served(a)=%d want 3 (one swap serves all waiters)", got)
	}
}

func TestFIFO_SwapDoneError_FailsAllWaiters(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateStopped
	s := newFIFO(&stubPlanner{}, eff)

	s.OnRequest(req("a"))
	s.OnRequest(req("a"))

	s.OnSwapDone(SwapDone{ModelID: "a", Err: errors.New("boom")})

	if eff.served("a") != 0 {
		t.Errorf("served(a)=%d want 0 on swap error", eff.served("a"))
	}
	if eff.errored("a") != 2 {
		t.Errorf("errored(a)=%d want 2 (both waiters fail)", eff.errored("a"))
	}
}

// TestFIFO_QueueOnEvictionCollision covers a request whose target evicts the
// model currently being swapped: it must queue until that swap finishes AND its
// served request drains, because starting it would stop a busy process.
func TestFIFO_QueueOnEvictionCollision(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateStopped
	eff.states["b"] = process.StateStopped
	// Loading b evicts a.
	s := newFIFO(&stubPlanner{evict: map[string][]string{"b": {"a"}}}, eff)

	s.OnRequest(req("a")) // StartSwap(a)
	s.OnRequest(req("b")) // collides with a's in-flight swap -> queue
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("b started early: StartSwap(b)=%d want 0", got)
	}

	// a becomes ready and is granted (now serving, inFlight[a]=1).
	eff.states["a"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "a"})
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("b started while a is serving: StartSwap(b)=%d want 0", got)
	}

	// a's request finishes -> a no longer in-flight -> b may now swap.
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	if got := eff.startsFor("b"); got != 1 {
		t.Fatalf("StartSwap(b)=%d want 1 after a drained", got)
	}
	if got := eff.starts[len(eff.starts)-1].evict; len(got) != 1 || got[0] != "a" {
		t.Errorf("b swap evict=%v want [a]", got)
	}
}

// TestFIFO_DisjointSwapsRunInParallel verifies two requests with
// non-conflicting evict sets both start without waiting for each other.
func TestFIFO_DisjointSwapsRunInParallel(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateStopped
	eff.states["b"] = process.StateStopped
	s := newFIFO(&stubPlanner{}, eff) // empty evicts

	s.OnRequest(req("a"))
	s.OnRequest(req("b"))

	if eff.startsFor("a") != 1 || eff.startsFor("b") != 1 {
		t.Fatalf("StartSwap a=%d b=%d want 1 each (parallel)", eff.startsFor("a"), eff.startsFor("b"))
	}
}

// TestFIFO_OverlappingEvictSetsDoNotRunInParallel verifies two swaps with
// different targets that evict the *same* model do not run concurrently: the
// second must queue rather than double-evict the shared model. Neither target is
// in the other's evict set, so this is only caught by the evict-set overlap
// check in collidesWith.
func TestFIFO_OverlappingEvictSetsDoNotRunInParallel(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateStopped
	eff.states["b"] = process.StateStopped
	eff.states["x"] = process.StateReady // shared eviction target, running
	// Loading a or b both require evicting x.
	s := newFIFO(&stubPlanner{evict: map[string][]string{"a": {"x"}, "b": {"x"}}}, eff)

	s.OnRequest(req("a")) // StartSwap(a, [x])
	s.OnRequest(req("b")) // overlaps a's evict set ([x]) -> queue
	if eff.startsFor("a") != 1 {
		t.Fatalf("StartSwap(a)=%d want 1", eff.startsFor("a"))
	}
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("b started in parallel while a evicts x: StartSwap(b)=%d want 0", got)
	}

	// a's swap completes and x is gone; b can now evict nothing and start.
	eff.states["a"] = process.StateReady
	eff.states["x"] = process.StateStopped
	s.OnSwapDone(SwapDone{ModelID: "a"})
	if got := eff.startsFor("b"); got != 1 {
		t.Fatalf("StartSwap(b)=%d want 1 after a's swap drained", got)
	}
}

// TestFIFO_QueueDrainPromotesMultiple verifies completing one swap unblocks
// every queued request that no longer collides — they all start together.
func TestFIFO_QueueDrainPromotesMultiple(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateStopped
	eff.states["b"] = process.StateStopped
	eff.states["c"] = process.StateStopped
	// a's swap evicts both b and c; b and c evict nothing.
	s := newFIFO(&stubPlanner{evict: map[string][]string{"a": {"b", "c"}}}, eff)

	s.OnRequest(req("a")) // StartSwap(a, [b,c])
	s.OnRequest(req("b")) // collides (in a's evict set) -> queue
	s.OnRequest(req("c")) // collides -> queue
	if eff.startsFor("b") != 0 || eff.startsFor("c") != 0 {
		t.Fatalf("b/c started early")
	}

	eff.states["a"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "a"})

	// b and c have empty evict sets and don't evict a, so both start now.
	if eff.startsFor("b") != 1 || eff.startsFor("c") != 1 {
		t.Fatalf("StartSwap b=%d c=%d want 1 each after a done", eff.startsFor("b"), eff.startsFor("c"))
	}
	if eff.served("a") != 1 {
		t.Errorf("served(a)=%d want 1", eff.served("a"))
	}
}

// TestFIFO_QueueCollation verifies duplicate requests collapse into one swap
// per model: the second request for each model joins the active swap (at arrival
// or at drain time) rather than triggering its own swap.
func TestFIFO_QueueCollation(t *testing.T) {
	eff := newFakeEffects()
	for _, id := range []string{"a", "b", "c"} {
		eff.states[id] = process.StateStopped
	}
	// Each model evicts the other two: all swaps are mutually exclusive.
	s := newFIFO(&stubPlanner{evict: map[string][]string{
		"a": {"b", "c"},
		"b": {"a", "c"},
		"c": {"a", "b"},
	}}, eff)

	for _, id := range []string{"a", "b", "c", "a", "b", "c"} {
		s.OnRequest(req(id))
	}

	// Drain a, then its served requests, which promotes b; repeat for b -> c.
	drain := func(model string, waiters int) {
		eff.states[model] = process.StateReady
		s.OnSwapDone(SwapDone{ModelID: model})
		for i := 0; i < waiters; i++ {
			s.OnServeDone(ServeDoneEvent{ModelID: model})
		}
	}
	drain("a", 2)
	drain("b", 2)
	drain("c", 2)

	for _, id := range []string{"a", "b", "c"} {
		if got := eff.startsFor(id); got != 1 {
			t.Errorf("StartSwap(%s)=%d want 1 (collation)", id, got)
		}
		if got := eff.served(id); got != 2 {
			t.Errorf("served(%s)=%d want 2", id, got)
		}
	}
}

// TestFIFO_NoSwapWhileServing verifies a model still handling requests is not
// evicted: the evicting request waits until every in-flight request drains.
func TestFIFO_NoSwapWhileServing(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	s := newFIFO(&stubPlanner{evict: map[string][]string{"b": {"a"}}}, eff)

	s.OnRequest(req("a")) // fast path, inFlight[a]=1
	s.OnRequest(req("a")) // fast path, inFlight[a]=2
	s.OnRequest(req("b")) // would evict busy a -> queue
	if eff.startsFor("b") != 0 {
		t.Fatalf("b started while a serving")
	}

	s.OnServeDone(ServeDoneEvent{ModelID: "a"}) // inFlight[a]=1
	if eff.startsFor("b") != 0 {
		t.Fatalf("b started while a still serving one request")
	}

	s.OnServeDone(ServeDoneEvent{ModelID: "a"}) // inFlight[a]=0
	if eff.startsFor("b") != 1 {
		t.Fatalf("StartSwap(b)=%d want 1 after a fully drained", eff.startsFor("b"))
	}
}

// TestFIFO_GrantServeFalseDoesNotLeakInFlight verifies that when a caller has
// walked away (GrantServe returns false) the in-flight count is not bumped, so a
// later evicting request is not blocked forever.
func TestFIFO_GrantServeFalseDoesNotLeakInFlight(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateStopped
	eff.states["b"] = process.StateStopped
	eff.serveResult["a"] = false // a's waiter is gone by grant time
	s := newFIFO(&stubPlanner{evict: map[string][]string{"b": {"a"}}}, eff)

	s.OnRequest(req("a"))
	eff.states["a"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "a"}) // grant fails, inFlight[a] stays 0

	// b evicts a; since a is not in-flight, b should start immediately.
	s.OnRequest(req("b"))
	if eff.startsFor("b") != 1 {
		t.Fatalf("StartSwap(b)=%d want 1 (no leaked in-flight on a)", eff.startsFor("b"))
	}
}

// TestFIFO_OnShutdown_FailsAllWaiters verifies shutdown errors every waiter the
// scheduler holds: active-swap waiters and queued requests alike.
func TestFIFO_OnShutdown_FailsAllWaiters(t *testing.T) {
	eff := newFakeEffects()
	for _, id := range []string{"a", "b", "c"} {
		eff.states[id] = process.StateStopped
	}
	// a and b load in parallel; c collides with both and queues.
	s := newFIFO(&stubPlanner{evict: map[string][]string{"c": {"a", "b"}}}, eff)

	s.OnRequest(req("a")) // StartSwap(a)
	s.OnRequest(req("a")) // join a
	s.OnRequest(req("b")) // StartSwap(b)
	s.OnRequest(req("b")) // join b
	s.OnRequest(req("c")) // queued

	s.OnShutdown(errors.New("shutting down"))

	if got := eff.errored(""); got != 5 {
		t.Errorf("error grants=%d want 5 (2 a + 2 b + 1 c)", got)
	}
}

func TestFIFO_OnUnload_ReleasesActiveWaiters(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateStopped
	s := newFIFO(&stubPlanner{}, eff)

	s.OnRequest(req("a")) // active swap a with one waiter
	s.OnRequest(req("a")) // join

	s.OnUnload([]string{"a"}, time.Second)

	if got := eff.errored("a"); got != 2 {
		t.Errorf("errored(a)=%d want 2 (active swap waiters released)", got)
	}
	if len(eff.stops) != 1 || len(eff.stops[0].ids) != 1 || eff.stops[0].ids[0] != "a" {
		t.Errorf("StopProcesses=%+v want one call stopping [a]", eff.stops)
	}
	if eff.stops[0].timeout != time.Second {
		t.Errorf("StopProcesses timeout=%v want 1s", eff.stops[0].timeout)
	}
}

func TestFIFO_OnUnload_DropsQueuedRequests(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateStopped
	eff.states["b"] = process.StateStopped
	// b evicts a, so a request for b queues while a is loading.
	s := newFIFO(&stubPlanner{evict: map[string][]string{"b": {"a"}}}, eff)

	s.OnRequest(req("a")) // StartSwap(a)
	s.OnRequest(req("b")) // queued

	s.OnUnload([]string{"b"}, time.Second)

	if got := eff.errored("b"); got != 1 {
		t.Errorf("errored(b)=%d want 1 (queued request dropped)", got)
	}
	if got := eff.startsFor("b"); got != 0 {
		t.Errorf("StartSwap(b)=%d want 0 (b should never start)", got)
	}
	// a's swap is untouched: its waiter is neither served nor errored yet.
	if eff.served("a") != 0 || eff.errored("a") != 0 {
		t.Errorf("a swap should be untouched: served=%d errored=%d", eff.served("a"), eff.errored("a"))
	}
}

// TestFIFO_PriorityQueueOrder verifies queued requests are ordered by descending
// priority, with arrival (FIFO) order preserved among equal-priority models.
func TestFIFO_PriorityQueueOrder(t *testing.T) {
	eff := newFakeEffects()
	for _, m := range []string{"z", "A", "B", "C", "D"} {
		eff.states[m] = process.StateStopped
	}
	// z's swap evicts every other model, so any request that arrives while z is
	// loading collides with z's in-flight swap and parks in the queue.
	planner := &stubPlanner{evict: map[string][]string{"z": {"A", "B", "C", "D"}}}
	cfg := config.FifoConfig{Priority: map[string]int{"A": 10, "B": 5, "C": 5, "D": 1}}
	s := NewFIFO("test", logmon.NewWriter(io.Discard), planner, cfg, nil, 0, 0, eff)

	s.OnRequest(req("z")) // StartSwap(z, [A,B,C,D])

	// Arrive out of priority order; B before C exercises FIFO tie-breaking.
	for _, m := range []string{"B", "D", "C", "A"} {
		s.OnRequest(req(m))
	}

	got := make([]string, len(s.queued))
	for i, q := range s.queued {
		got[i] = q.Model
	}
	want := []string{"A", "B", "C", "D"}
	if len(got) != len(want) {
		t.Fatalf("queue=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("queue=%v want %v", got, want)
		}
	}
}

// TestFIFO_OnCancel_QueuedRequest verifies that cancelling a queued request
// prevents drainQueue from ever starting a model load for it. Without OnCancel
// the dead request would sit in the queue until a drain triggers a wasted swap.
func TestFIFO_OnCancel_QueuedRequest(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateStopped
	eff.states["b"] = process.StateStopped
	// b evicts a, so a request for b queues while a is loading.
	s := newFIFO(&stubPlanner{evict: map[string][]string{"b": {"a"}}}, eff)

	s.OnRequest(req("a")) // StartSwap(a)

	cancelledReq := reqCh("b")
	s.OnRequest(cancelledReq) // queued (collides with a's in-flight swap)
	if len(s.queued) != 1 {
		t.Fatalf("queue len=%d want 1 before cancel", len(s.queued))
	}

	// Client disconnects.
	s.OnCancel(cancelledReq)

	if len(s.queued) != 0 {
		t.Fatalf("queue len=%d want 0 after cancel", len(s.queued))
	}

	// a's swap finishes; drainQueue runs but b is gone — no swap for b.
	eff.states["a"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "a"})

	if got := eff.startsFor("b"); got != 0 {
		t.Errorf("StartSwap(b)=%d want 0 (cancelled request should not trigger a load)", got)
	}
}

// TestFIFO_OnCancel_SwapWaiter verifies that cancelling a request that joined an
// in-flight swap removes it from the waiter list. When the swap completes, the
// cancelled waiter receives no grant and does not bump the in-flight count.
func TestFIFO_OnCancel_SwapWaiter(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateStopped
	s := newFIFO(&stubPlanner{}, eff)

	liveReq := reqCh("a")
	cancelledReq := reqCh("a")
	s.OnRequest(liveReq)      // starts swap
	s.OnRequest(cancelledReq) // joins

	if sw := s.active["a"]; len(sw.waiters) != 2 {
		t.Fatalf("waiters=%d want 2", len(sw.waiters))
	}

	s.OnCancel(cancelledReq)

	if sw := s.active["a"]; len(sw.waiters) != 1 {
		t.Fatalf("waiters=%d want 1 after cancel", len(sw.waiters))
	}

	// Swap finishes: only the live waiter is granted.
	eff.states["a"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "a"})

	if got := eff.served("a"); got != 1 {
		t.Errorf("served(a)=%d want 1 (only the non-cancelled waiter)", got)
	}
}

// TestFIFO_OnCancel_NotPresent is a no-op: cancelling a request that was already
// granted (and is no longer queued or waiting) must not affect anything.
func TestFIFO_OnCancel_NotPresent(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	s := newFIFO(&stubPlanner{}, eff)

	r := reqCh("a")
	s.OnRequest(r) // fast-path served immediately

	// Cancel after grant — should be a harmless no-op.
	s.OnCancel(r)

	if got := eff.served("a"); got != 1 {
		t.Errorf("served(a)=%d want 1 (cancel of granted request is a no-op)", got)
	}
	if len(s.queued) != 0 {
		t.Errorf("queue should be empty, len=%d", len(s.queued))
	}
}

// newFIFOWithLimit builds a FIFO whose single model has the given concurrency
// limit, already in StateReady so every request exercises the fast path.
func newFIFOWithLimit(t *testing.T, model string, limit int) (*FIFO, *fakeEffects) {
	t.Helper()
	eff := newFakeEffects()
	eff.states[model] = process.StateReady
	models := map[string]config.ModelConfig{
		model: {ConcurrencyLimit: limit},
	}
	s := NewFIFO("test", logmon.NewWriter(io.Discard), &stubPlanner{}, config.FifoConfig{}, models, 0, 0, eff)
	return s, eff
}

// TestFIFO_ConcurrencyLimit_RejectsOverLimit verifies that a request arriving
// while the model is at capacity gets rejected during admission, before it can
// be queued or served, and that a new request succeeds once capacity returns.
func TestFIFO_ConcurrencyLimit_RejectsOverLimit(t *testing.T) {
	s, eff := newFIFOWithLimit(t, "a", 1)

	// First request: served (inFlight 0 → 1).
	r1 := req("a")
	s.OnRequest(r1)
	assertAdmitted(t, r1)
	if got := eff.served("a"); got != 1 {
		t.Fatalf("served(a)=%d want 1", got)
	}

	// Second request while slot is occupied: rejected at admission with 429.
	r2 := req("a")
	s.OnRequest(r2)
	assertAdmission429(t, r2)
	if got := eff.errored("a"); got != 0 {
		t.Fatalf("errored(a)=%d want 0 (over-limit rejects before grant)", got)
	}

	// After the in-flight request finishes, a new request succeeds.
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	r3 := req("a")
	s.OnRequest(r3)
	assertAdmitted(t, r3)
	if got := eff.served("a"); got != 2 {
		t.Fatalf("served(a)=%d want 2 after drain", got)
	}
}

// TestFIFO_ConcurrencyLimit_DefaultIsTen verifies that a model without an
// explicit ConcurrencyLimit gets the default cap of 10.
func TestFIFO_ConcurrencyLimit_DefaultIsTen(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateReady
	// nil models → every model gets defaultConcurrencyLimit (10).
	s := newFIFO(&stubPlanner{}, eff)

	for i := 0; i < 10; i++ {
		r := req("a")
		s.OnRequest(r)
		assertAdmitted(t, r)
	}
	if got := eff.served("a"); got != 10 {
		t.Fatalf("served(a)=%d want 10 (default limit)", got)
	}

	// 11th request is rejected.
	r := req("a")
	s.OnRequest(r)
	assertAdmission429(t, r)
	if got := eff.errored("a"); got != 0 {
		t.Fatalf("errored(a)=%d want 0 (over default limit rejects before grant)", got)
	}
}

// TestFIFO_ConcurrencyLimit_CustomLimit verifies a ConcurrencyLimit greater
// than zero overrides the default.
func TestFIFO_ConcurrencyLimit_CustomLimit(t *testing.T) {
	s, eff := newFIFOWithLimit(t, "a", 2)

	r1 := req("a")
	r2 := req("a")
	r3 := req("a")
	s.OnRequest(r1)
	s.OnRequest(r2)
	s.OnRequest(r3)
	assertAdmitted(t, r1)
	assertAdmitted(t, r2)
	assertAdmission429(t, r3)

	if got := eff.served("a"); got != 2 {
		t.Fatalf("served(a)=%d want 2 (custom limit)", got)
	}
	if got := eff.errored("a"); got != 0 {
		t.Fatalf("errored(a)=%d want 0 (over custom limit rejects before grant)", got)
	}
}

// TestFIFO_ConcurrencyLimit_SwapWaiters verifies that when more swap waiters
// exist than the concurrency limit, excess waiters are rejected during
// admission rather than after the loading stream has started.
func TestFIFO_ConcurrencyLimit_SwapWaiters(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateStopped
	models := map[string]config.ModelConfig{
		"a": {ConcurrencyLimit: 2},
	}
	s := NewFIFO("test", logmon.NewWriter(io.Discard), &stubPlanner{}, config.FifoConfig{}, models, 0, 0, eff)

	// Three requests arrive while model is loading: one starts swap, two join.
	r1 := req("a")
	r2 := req("a")
	r3 := req("a")
	s.OnRequest(r1)
	s.OnRequest(r2)
	s.OnRequest(r3)
	assertAdmitted(t, r1)
	assertAdmitted(t, r2)
	assertAdmission429(t, r3)

	if got := eff.startsFor("a"); got != 1 {
		t.Fatalf("StartSwap(a)=%d want 1", got)
	}
	if sw := s.active["a"]; len(sw.waiters) != 2 {
		t.Fatalf("waiters=%d want 2 (third request must not join)", len(sw.waiters))
	}

	// Swap completes: only the two admitted requests are served.
	eff.states["a"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "a"})

	if got := eff.served("a"); got != 2 {
		t.Fatalf("served(a)=%d want 2", got)
	}
	if got := eff.errored("a"); got != 0 {
		t.Fatalf("errored(a)=%d want 0 (excess waiter rejected at admission)", got)
	}
}

func TestFIFO_ConcurrencyLimit_QueuedWaitersReserveCapacity(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateStopped
	eff.states["b"] = process.StateStopped
	models := map[string]config.ModelConfig{
		"a": {ConcurrencyLimit: 2},
		"b": {},
	}
	s := NewFIFO("test", logmon.NewWriter(io.Discard), &stubPlanner{evict: map[string][]string{"a": {"b"}}}, config.FifoConfig{}, models, 0, 0, eff)

	bReq := req("b")
	aReq1 := req("a")
	aReq2 := req("a")
	aReq3 := req("a")

	s.OnRequest(bReq)  // StartSwap(b)
	s.OnRequest(aReq1) // queued behind b
	s.OnRequest(aReq2) // queued behind b
	s.OnRequest(aReq3) // rejected before queueing

	assertAdmitted(t, bReq)
	assertAdmitted(t, aReq1)
	assertAdmitted(t, aReq2)
	assertAdmission429(t, aReq3)

	if got := len(s.queued); got != 2 {
		t.Fatalf("queue len=%d want 2", got)
	}
	if got := eff.startsFor("a"); got != 0 {
		t.Fatalf("StartSwap(a)=%d want 0 while b is loading", got)
	}

	eff.states["b"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "b"})
	s.OnServeDone(ServeDoneEvent{ModelID: "b"})
	if got := eff.startsFor("a"); got != 1 {
		t.Fatalf("StartSwap(a)=%d want 1 after b drains", got)
	}

	eff.states["a"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "a"})
	if got := eff.served("a"); got != 2 {
		t.Fatalf("served(a)=%d want 2", got)
	}
}

func TestFIFO_ConcurrencyLimit_CancelledQueuedWaiterReleasesReservation(t *testing.T) {
	eff := newFakeEffects()
	eff.states["a"] = process.StateStopped
	eff.states["b"] = process.StateStopped
	models := map[string]config.ModelConfig{
		"a": {ConcurrencyLimit: 1},
		"b": {},
	}
	s := NewFIFO("test", logmon.NewWriter(io.Discard), &stubPlanner{evict: map[string][]string{"a": {"b"}}}, config.FifoConfig{}, models, 0, 0, eff)

	bReq := req("b")
	cancelledReq := reqCh("a")
	rejectedReq := req("a")
	retryReq := req("a")

	s.OnRequest(bReq)
	s.OnRequest(cancelledReq)
	s.OnRequest(rejectedReq)
	assertAdmitted(t, bReq)
	assertAdmitted(t, cancelledReq)
	assertAdmission429(t, rejectedReq)

	s.OnCancel(cancelledReq)
	s.OnRequest(retryReq)
	assertAdmitted(t, retryReq)

	if got := len(s.queued); got != 1 {
		t.Fatalf("queue len=%d want 1 after cancel and retry", got)
	}
}

// ── Memory admission (Phase 1) ──────────────────────────────────────────────

func newFIFOMem(t *testing.T, planner Swapper, models map[string]config.ModelConfig, pool, reserve int64) (*FIFO, *fakeEffects) {
	t.Helper()
	eff := newFakeEffects()
	return NewFIFO("test", logmon.NewWriter(io.Discard), planner, config.FifoConfig{}, models, pool, reserve, eff), eff
}

// assertMemoryRefused asserts a 503 memory refusal was delivered to model via
// GrantError (the post-admission path), not via the admission channel.
func assertMemoryRefused(t *testing.T, eff *fakeEffects, model string) {
	t.Helper()
	for _, g := range eff.grants {
		if g.model == model && g.err != nil {
			var me swaputil.MemoryAdmissionError
			if errors.As(g.err, &me) {
				return
			}
		}
	}
	t.Fatalf("expected a MemoryAdmissionError refusal for %s; grants=%+v", model, eff.grants)
}

func adoptReq(model string) HandlerReq {
	r := req(model)
	r.Ctx = swaputil.SetContext(context.Background(), swaputil.ReqContextData{Model: model, ModelID: model, Metadata: map[string]string{"adopt": "1"}})
	return r
}

func TestFIFO_Memory_AdoptBypassesGate(t *testing.T) {
	// An adopt attach to an over-budget model must NOT be refused: it attaches to
	// an already-running container and spends no new memory. Gating it would 503 a
	// live model and leave it invisible to the ledger (the OOM-#2 restart path).
	models := map[string]config.ModelConfig{"big": {MemoryCeiling: 500}} // 500 > pool 100
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["big"] = process.StateStopped
	s.OnRequest(adoptReq("big"))
	if eff.errored("big") != 0 {
		t.Fatalf("adopt refused by the memory gate; errored=%d want 0", eff.errored("big"))
	}
	if eff.startsFor("big") != 1 {
		t.Fatalf("startsFor(big)=%d want 1 (adopt must proceed to attach)", eff.startsFor("big"))
	}
}

func TestFIFO_Memory_EvictCreditAllowsSwap(t *testing.T) {
	// b resident (60), a wants in (60), pool 100: a+b=120 won't fit, but the
	// planner evicts b, so a fits once the eviction is credited.
	models := map[string]config.ModelConfig{
		"a": {MemoryCeiling: 60},
		"b": {MemoryCeiling: 60},
	}
	s, eff := newFIFOMem(t, &stubPlanner{evict: map[string][]string{"a": {"b"}}}, models, 100, 0)
	eff.states["b"] = process.StateReady
	eff.states["a"] = process.StateStopped
	s.OnRequest(reqCh("a"))
	if eff.startsFor("a") != 1 {
		t.Fatalf("startsFor(a)=%d want 1 (a fits once b is evicted)", eff.startsFor("a"))
	}
}

func TestFIFO_Memory_ReserveReducesBudget(t *testing.T) {
	// pool 100, reserve 30 → budget 70. a(60) fits alone, but with small(20)
	// resident and not evicted, 60+20=80 > 70 → a does not fit (not never-fits).
	// small is Stopping so something is pending and a queues rather than 503s.
	models := map[string]config.ModelConfig{
		"a":     {MemoryCeiling: 60},
		"small": {MemoryCeiling: 20},
	}
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 30)
	eff.states["small"] = process.StateStopping
	eff.states["a"] = process.StateStopped
	s.OnRequest(reqCh("a"))
	if eff.startsFor("a") != 0 || len(s.queued) != 1 {
		t.Fatalf("startsFor(a)=%d queued=%d want 0/1 (60+20=80 > budget 70)", eff.startsFor("a"), len(s.queued))
	}
}

func TestFIFO_Memory_AdmitsWhenFits(t *testing.T) {
	models := map[string]config.ModelConfig{"a": {MemoryCeiling: 50}}
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["a"] = process.StateStopped // known, not running
	r := req("a")
	s.OnRequest(r)
	assertAdmitted(t, r)
	if eff.startsFor("a") != 1 {
		t.Fatalf("startsFor(a)=%d want 1", eff.startsFor("a"))
	}
}

// TestFIFO_Memory_NeverFitsRefusedAtAdmission covers todo 1.7 (C1) at the
// scheduler level: a never-fits load is refused on the admission channel, before
// admit() succeeds, so the router never lets a streaming caller commit a 200 and
// has to frame the 503 into an SSE stream. Nothing is reserved, so nothing needs
// releasing.
func TestFIFO_Memory_NeverFitsRefusedAtAdmission(t *testing.T) {
	models := map[string]config.ModelConfig{"a": {MemoryCeiling: 200}} // 200 > pool 100
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["a"] = process.StateStopped
	r := req("a")
	s.OnRequest(r)
	assertAdmission503(t, r)
	if eff.errored("a") != 0 {
		t.Fatalf("errored(a)=%d want 0 (refusal must not go through the post-admission grant path)", eff.errored("a"))
	}
	if eff.startsFor("a") != 0 {
		t.Fatalf("startsFor(a)=%d want 0", eff.startsFor("a"))
	}
	if len(s.reserved) != 0 {
		t.Fatalf("reserved=%v want empty (nothing may be reserved for a refused request)", s.reserved)
	}
}

func TestFIFO_Memory_UnknownCeilingRefused(t *testing.T) {
	models := map[string]config.ModelConfig{"a": {}} // no ceiling configured
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["a"] = process.StateStopped
	r := req("a")
	s.OnRequest(r)
	assertAdmission503(t, r)
	if eff.startsFor("a") != 0 {
		t.Fatalf("startsFor(a)=%d want 0", eff.startsFor("a"))
	}
	if len(s.reserved) != 0 {
		t.Fatalf("reserved=%v want empty", s.reserved)
	}
}

// TestFIFO_Memory_ResidentModelIsNotRefused guards the pre-admission refusal's
// scope: it applies to NEW loads only. A model that is already resident (fast
// path) keeps being served even if its configured ceiling could not be admitted
// today — refusing there would strand a live model behind a 503.
func TestFIFO_Memory_ResidentModelIsNotRefused(t *testing.T) {
	models := map[string]config.ModelConfig{"a": {MemoryCeiling: 200}} // > pool
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["a"] = process.StateReady
	r := req("a")
	s.OnRequest(r)
	assertAdmitted(t, r)
	if eff.served("a") != 1 {
		t.Fatalf("served(a)=%d want 1 (a resident model must still be served)", eff.served("a"))
	}
}

// TestFIFO_Memory_JoiningSwapIsNotRefused is the other half of that scope: a
// second caller for a model whose swap is already in flight joins it rather than
// being re-gated — the memory for that load was admitted when the swap started.
func TestFIFO_Memory_JoiningSwapIsNotRefused(t *testing.T) {
	models := map[string]config.ModelConfig{"a": {MemoryCeiling: 60}}
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["a"] = process.StateStopped
	s.OnRequest(reqCh("a"))
	if eff.startsFor("a") != 1 {
		t.Fatalf("startsFor(a)=%d want 1", eff.startsFor("a"))
	}
	second := reqCh("a")
	s.OnRequest(second)
	assertAdmitted(t, second)
	if got := len(s.active["a"].waiters); got != 2 {
		t.Fatalf("waiters=%d want 2 (second caller must join the in-flight swap)", got)
	}
}

func TestFIFO_Memory_PoolZeroIsNoOp(t *testing.T) {
	models := map[string]config.ModelConfig{"a": {}} // no ceiling, but gate off
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 0, 0)
	eff.states["a"] = process.StateStopped
	r := req("a")
	s.OnRequest(r)
	assertAdmitted(t, r)
	if eff.startsFor("a") != 1 {
		t.Fatalf("startsFor(a)=%d want 1 (gate off must admit)", eff.startsFor("a"))
	}
}

func TestFIFO_Memory_TransientOverQueuesThenDrains(t *testing.T) {
	// a and b fit alone (60 each) but not together (120 > 100). b is resident and
	// not evicted; a request for a queues, then starts once b frees and drains.
	models := map[string]config.ModelConfig{
		"a": {MemoryCeiling: 60},
		"b": {MemoryCeiling: 60},
	}
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["b"] = process.StateStopping // resident, being stopped (pending)
	eff.states["a"] = process.StateStopped  // wanted
	r := reqCh("a")
	s.OnRequest(r)
	assertAdmitted(t, r)
	if eff.startsFor("a") != 0 || len(s.queued) != 1 {
		t.Fatalf("startsFor(a)=%d queued=%d want 0/1 (should queue)", eff.startsFor("a"), len(s.queued))
	}
	delete(eff.states, "b") // b leaves → memory frees
	s.drainQueue()
	if eff.startsFor("a") != 1 || len(s.queued) != 0 {
		t.Fatalf("startsFor(a)=%d queued=%d want 1/0 after b freed", eff.startsFor("a"), len(s.queued))
	}
}

func TestFIFO_Memory_QueuedLoadLogsWarning(t *testing.T) {
	// A memory-queued load must leave a visible trace naming what it needs and
	// what is holding the budget, or a request waiting on a slow stop waits
	// silently.
	models := map[string]config.ModelConfig{
		"a": {MemoryCeiling: 60},
		"b": {MemoryCeiling: 70},
	}
	logger := logmon.NewWriter(io.Discard)
	eff := newFakeEffects()
	s := NewFIFO("test", logger, &stubPlanner{}, config.FifoConfig{}, models, 110, 10, eff)
	eff.states["b"] = process.StateStopping
	eff.states["a"] = process.StateStopped
	s.OnRequest(reqCh("a"))
	if len(s.queued) != 1 {
		t.Fatalf("queued=%d want 1", len(s.queued))
	}
	got := string(logger.GetHistory())
	want := "[WARN] test: queuing model a (does not fit now; waiting for memory to free): needs 60 bytes, budget 100 (pool 110 - reserve 10), still resident after eviction: b=70"
	if !strings.Contains(got, want) {
		t.Errorf("log missing %q; got:\n%s", want, got)
	}
}

func TestFIFO_Memory_TOCTOU_SecondLoadCannotAlsoPass(t *testing.T) {
	// a and b fit alone (60) but not together. a starts a swap (its target enters
	// the active set), so b's fit check must count a as resident-to-be and NOT start.
	models := map[string]config.ModelConfig{
		"a": {MemoryCeiling: 60},
		"b": {MemoryCeiling: 60},
	}
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["a"] = process.StateStopped
	eff.states["b"] = process.StateStopped
	s.OnRequest(reqCh("a"))
	if eff.startsFor("a") != 1 {
		t.Fatalf("startsFor(a)=%d want 1", eff.startsFor("a"))
	}
	s.OnRequest(reqCh("b"))
	if eff.startsFor("b") != 0 {
		t.Fatalf("startsFor(b)=%d want 0 (a's in-flight target must count against b)", eff.startsFor("b"))
	}
}

func TestFIFO_Memory_DrainDropsNeverFitsAndReleases(t *testing.T) {
	// Defensive: a never-fits request sitting in the queue is dropped with a 503
	// on drain and its reservation released, never stranded in remaining.
	models := map[string]config.ModelConfig{"big": {MemoryCeiling: 200}} // > pool
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["big"] = process.StateStopped
	r := reqCh("big")
	if !s.admit(r) { // reserve a slot as OnRequest would
		t.Fatal("admit failed")
	}
	s.enqueue(r) // place it in the queue directly
	s.drainQueue()
	assertMemoryRefused(t, eff, "big")
	if len(s.queued) != 0 {
		t.Fatalf("queued=%d want 0 (never-fits must be dropped)", len(s.queued))
	}
	if len(s.reserved) != 0 {
		t.Fatalf("reserved=%v want empty (slot must be released)", s.reserved)
	}
}

// TestFIFO_Memory_BlockedWithNothingPendingRefusedAtAdmission is the P1 regression,
// shaped like the live GB10 config: parakeet-asr (8) + qwen-asr (14) stay
// resident (asr group, never evicted for an sglang load) beside one sglang model;
// flash (100) evicts only the sglang model, and 100+22 > the 111 budget. Nothing
// is in flight that could ever free the ASR pair, so queuing would wait forever
// (and stream "Queue position" to a streaming caller forever). It must be refused
// on the admission channel, naming the models that hold the budget.
func TestFIFO_Memory_BlockedWithNothingPendingRefusedAtAdmission(t *testing.T) {
	models := map[string]config.ModelConfig{
		"parakeet-asr": {MemoryCeiling: 8},
		"qwen-asr":     {MemoryCeiling: 14},
		"qwen38-27b":   {MemoryCeiling: 83},
		"flash-vllm":   {MemoryCeiling: 100},
	}
	s, eff := newFIFOMem(t, &stubPlanner{evict: map[string][]string{"flash-vllm": {"qwen38-27b"}}}, models, 121, 10)
	eff.states["parakeet-asr"] = process.StateReady
	eff.states["qwen-asr"] = process.StateReady
	eff.states["qwen38-27b"] = process.StateReady
	eff.states["flash-vllm"] = process.StateStopped

	r := reqCh("flash-vllm")
	s.OnRequest(r)
	err := admitErr(t, r)
	var memErr swaputil.MemoryAdmissionError
	if !errors.As(err, &memErr) {
		t.Fatalf("admission err=%v want MemoryAdmissionError", err)
	}
	for _, want := range []string{`"flash-vllm" needs 100 bytes`, "budget of 111", "parakeet-asr=8", "qwen-asr=14"} {
		if !strings.Contains(memErr.Message, want) {
			t.Errorf("refusal message %q missing %q", memErr.Message, want)
		}
	}
	if strings.Contains(memErr.Message, "qwen38-27b") {
		t.Errorf("refusal message %q names the model being evicted as a holder", memErr.Message)
	}
	if eff.startsFor("flash-vllm") != 0 || len(s.queued) != 0 || len(s.reserved) != 0 {
		t.Fatalf("starts=%d queued=%d reserved=%v want 0/0/empty", eff.startsFor("flash-vllm"), len(s.queued), s.reserved)
	}
}

// TestFIFO_Memory_QueuesBehindEvictingSwapThenServed: a request that does not
// fit only because an in-flight swap is still evicting must queue (not 503) and
// start once that swap lands. a (60) is being evicted by b's swap (b 30); c (40)
// is in a group that evicts nothing, and a+b+c = 130 > 100 while a is counted.
func TestFIFO_Memory_QueuesBehindEvictingSwapThenServed(t *testing.T) {
	models := map[string]config.ModelConfig{
		"a": {MemoryCeiling: 60},
		"b": {MemoryCeiling: 30},
		"c": {MemoryCeiling: 40},
	}
	s, eff := newFIFOMem(t, &stubPlanner{evict: map[string][]string{"b": {"a"}}}, models, 100, 0)
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	eff.states["c"] = process.StateStopped

	s.OnRequest(reqCh("b"))
	if eff.startsFor("b") != 1 {
		t.Fatalf("startsFor(b)=%d want 1", eff.startsFor("b"))
	}
	eff.states["a"] = process.StateStopping // b's swap is stopping a

	c := reqCh("c")
	s.OnRequest(c)
	assertAdmitted(t, c)
	if eff.startsFor("c") != 0 || len(s.queued) != 1 {
		t.Fatalf("startsFor(c)=%d queued=%d want 0/1 (must wait for b's eviction)", eff.startsFor("c"), len(s.queued))
	}

	eff.states["a"] = process.StateStopped
	eff.states["b"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "b"})
	if eff.startsFor("c") != 1 || len(s.queued) != 0 {
		t.Fatalf("startsFor(c)=%d queued=%d want 1/0 after the eviction landed (30+40 fits)", eff.startsFor("c"), len(s.queued))
	}
	if eff.errored("c") != 0 {
		t.Fatalf("errored(c)=%d want 0", eff.errored("c"))
	}
}

// TestFIFO_Memory_DrainRefusesWhenNothingPending: a request queued while
// something was pending is refused (not re-queued forever) once that settles
// and it still does not fit. It was already admitted, so the refusal goes
// through GrantError and its reservation is released.
func TestFIFO_Memory_DrainRefusesWhenNothingPending(t *testing.T) {
	models := map[string]config.ModelConfig{
		"a": {MemoryCeiling: 60},
		"b": {MemoryCeiling: 30},
		"c": {MemoryCeiling: 80},
	}
	s, eff := newFIFOMem(t, &stubPlanner{evict: map[string][]string{"b": {"a"}}}, models, 100, 0)
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	eff.states["c"] = process.StateStopped

	s.OnRequest(reqCh("b"))
	c := reqCh("c")
	s.OnRequest(c)
	assertAdmitted(t, c)
	if len(s.queued) != 1 {
		t.Fatalf("queued=%d want 1 (b's swap is pending)", len(s.queued))
	}

	eff.states["a"] = process.StateStopped
	eff.states["b"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "b"}) // 30 + 80 = 110 > 100, nothing pending
	assertMemoryRefused(t, eff, "c")
	if len(s.queued) != 0 || s.reserved["c"] != 0 {
		t.Fatalf("queued=%d reserved=%v want 0 and no slot held for c", len(s.queued), s.reserved)
	}
	if eff.startsFor("c") != 0 {
		t.Fatalf("startsFor(c)=%d want 0", eff.startsFor("c"))
	}
}

// ── Leaked (force-killed) models ────────────────────────────────────────────

// TestFIFO_Memory_ForcedKillEvicteeChargedUntilGone is the P2 regression at the
// scheduler level. b's swap force-killed its evictee a: the process reads
// Stopped, but its container may still hold 60. A retry of b used to see a as
// gone, credit it and load on top. It must now queue (the leak is pending),
// and start only once the leak watcher reports a's upstream gone.
func TestFIFO_Memory_ForcedKillEvicteeChargedUntilGone(t *testing.T) {
	models := map[string]config.ModelConfig{
		"a": {MemoryCeiling: 60},
		"b": {MemoryCeiling: 60},
	}
	s, eff := newFIFOMem(t, &stubPlanner{evict: map[string][]string{"b": {"a"}}}, models, 100, 0)
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped

	s.OnRequest(reqCh("b"))
	eff.states["a"] = process.StateStopped // forced kill: a real process ends Stopped
	s.OnSwapDone(SwapDone{ModelID: "b", Err: errors.New("eviction did not complete"), Leaked: []string{"a"}})
	if _, ok := s.leaked["a"]; !ok || !eff.watching["a"] {
		t.Fatalf("leaked=%v watching=%v want a leaked and watched", s.leaked, eff.watching)
	}

	retry := reqCh("b")
	s.OnRequest(retry)
	assertAdmitted(t, retry)
	if eff.startsFor("b") != 1 || len(s.queued) != 1 {
		t.Fatalf("startsFor(b)=%d queued=%d want 1/1 (retry must wait for the leak, not load on top)", eff.startsFor("b"), len(s.queued))
	}

	s.OnLeakGone("a", "test")
	if _, ok := s.leaked["a"]; ok || eff.watching["a"] {
		t.Fatalf("leaked=%v watching=%v want a cleared", s.leaked, eff.watching)
	}
	if eff.startsFor("b") != 2 || len(s.queued) != 0 {
		t.Fatalf("startsFor(b)=%d queued=%d want 2/0 once a's upstream is gone", eff.startsFor("b"), len(s.queued))
	}
}

// TestFIFO_Memory_InFlightEvicteeChargedUntilSwapDone closes the window between
// an evictee's Stop returning (process reads Stopped) and its SwapDone saying
// whether that stop was forced: a load decided in that window must still count
// the evictee.
func TestFIFO_Memory_InFlightEvicteeChargedUntilSwapDone(t *testing.T) {
	models := map[string]config.ModelConfig{
		"a": {MemoryCeiling: 60},
		"b": {MemoryCeiling: 30},
		"c": {MemoryCeiling: 40},
	}
	s, eff := newFIFOMem(t, &stubPlanner{evict: map[string][]string{"b": {"a"}}}, models, 90, 0) // 90: c never fits beside a (60+40)
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	eff.states["c"] = process.StateStopped

	s.OnRequest(reqCh("b"))
	eff.states["a"] = process.StateStopped // a's Stop returned; SwapDone not yet seen

	s.OnRequest(reqCh("c"))
	if eff.startsFor("c") != 0 || len(s.queued) != 1 {
		t.Fatalf("startsFor(c)=%d queued=%d want 0/1 (a still counts until b's swap reports)", eff.startsFor("c"), len(s.queued))
	}

	s.OnSwapDone(SwapDone{ModelID: "b", Err: errors.New("eviction did not complete"), Leaked: []string{"a"}})
	if eff.startsFor("c") != 0 || len(s.queued) != 1 {
		t.Fatalf("startsFor(c)=%d queued=%d want 0/1 (a leaked: keep waiting)", eff.startsFor("c"), len(s.queued))
	}
	s.OnLeakGone("a", "test")
	if eff.startsFor("c") != 1 {
		t.Fatalf("startsFor(c)=%d want 1 after a's leak cleared", eff.startsFor("c"))
	}
}

// TestFIFO_Memory_UnloadForcedKillRecordsLeak: an unload whose stop force-killed
// keeps charging the model, and a load that only fails to fit because of it
// queues (the leak is pending) instead of being refused.
func TestFIFO_Memory_UnloadForcedKillRecordsLeak(t *testing.T) {
	models := map[string]config.ModelConfig{
		"a": {MemoryCeiling: 60},
		"c": {MemoryCeiling: 60},
	}
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["a"] = process.StateReady
	eff.states["c"] = process.StateStopped
	eff.forced["a"] = true

	s.OnUnload([]string{"a"}, time.Second)
	if _, ok := s.leaked["a"]; !ok || !eff.watching["a"] {
		t.Fatalf("leaked=%v watching=%v want a leaked and watched", s.leaked, eff.watching)
	}

	r := reqCh("c")
	s.OnRequest(r)
	assertAdmitted(t, r)
	if eff.startsFor("c") != 0 || len(s.queued) != 1 {
		t.Fatalf("startsFor(c)=%d queued=%d want 0/1", eff.startsFor("c"), len(s.queued))
	}
	s.OnLeakGone("a", "test")
	if eff.startsFor("c") != 1 {
		t.Fatalf("startsFor(c)=%d want 1 after the leak cleared", eff.startsFor("c"))
	}
}

// TestFIFO_Memory_LeakClearedWhenModelStartsAgain: once a leaked model is
// started (or adopted) successfully it is counted through the running set, so
// the leak entry and its watch go away.
func TestFIFO_Memory_LeakClearedWhenModelStartsAgain(t *testing.T) {
	models := map[string]config.ModelConfig{"a": {MemoryCeiling: 60}}
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["a"] = process.StateReady
	eff.forced["a"] = true
	s.OnUnload([]string{"a"}, time.Second)

	s.OnRequest(adoptReq("a"))
	if eff.startsFor("a") != 1 {
		t.Fatalf("startsFor(a)=%d want 1 (adopt bypasses the gate, including the model's own leak)", eff.startsFor("a"))
	}
	eff.states["a"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "a"})
	if _, ok := s.leaked["a"]; ok || eff.watching["a"] {
		t.Fatalf("leaked=%v watching=%v want cleared after a successful start", s.leaked, eff.watching)
	}
}

// TestFIFO_Memory_LeaksIgnoredWithGateOff: with memoryPool 0 nothing is
// charged, so a forced kill records nothing and starts no watcher.
func TestFIFO_Memory_LeaksIgnoredWithGateOff(t *testing.T) {
	s, eff := newFIFOMem(t, &stubPlanner{}, map[string]config.ModelConfig{"a": {}}, 0, 0)
	eff.states["a"] = process.StateReady
	eff.forced["a"] = true
	s.OnUnload([]string{"a"}, time.Second)
	if len(s.leaked) != 0 || len(eff.watching) != 0 {
		t.Fatalf("leaked=%v watching=%v want none with the gate off", s.leaked, eff.watching)
	}
}

// TestFIFO_Memory_StaleSwapDoneKeepsUnloadLeak: b's EnsureReady succeeded, but
// before its SwapDone reached the run loop an unload (UI "Cancel load", or a
// TTL) force-killed b and recorded the leak. The late SwapDone{Err: nil} is
// stale and must not erase that leak while b's container may still be up.
func TestFIFO_Memory_StaleSwapDoneKeepsUnloadLeak(t *testing.T) {
	models := map[string]config.ModelConfig{"b": {MemoryCeiling: 60}}
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["b"] = process.StateStopped
	eff.forced["b"] = true

	s.OnRequest(reqCh("b"))
	eff.states["b"] = process.StateReady // EnsureReady returned nil
	s.OnUnload([]string{"b"}, time.Second)
	if _, ok := s.leaked["b"]; !ok {
		t.Fatalf("leaked=%v want b recorded by the forced unload", s.leaked)
	}

	s.OnSwapDone(SwapDone{ModelID: "b"})
	if _, ok := s.leaked["b"]; !ok || !eff.watching["b"] {
		t.Fatalf("leaked=%v watching=%v want b still leaked and watched after a stale SwapDone", s.leaked, eff.watching)
	}
}

// TestFIFO_Memory_SwapDoneForStoppedModelKeepsLeak: the swap still owns its
// entry, but the model is no longer Ready when SwapDone is handled (it stopped
// on its own in the gap), so the success says nothing about the leaked
// container being gone.
func TestFIFO_Memory_SwapDoneForStoppedModelKeepsLeak(t *testing.T) {
	models := map[string]config.ModelConfig{"a": {MemoryCeiling: 60}}
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["a"] = process.StateReady
	eff.forced["a"] = true
	s.OnUnload([]string{"a"}, time.Second) // a leaked

	s.OnRequest(adoptReq("a")) // owns an active entry
	eff.states["a"] = process.StateStopped
	s.OnSwapDone(SwapDone{ModelID: "a"})
	if _, ok := s.leaked["a"]; !ok {
		t.Fatalf("leaked=%v want a kept: it was not Ready when its SwapDone arrived", s.leaked)
	}
}

// TestFIFO_Memory_LeakedTargetWaitsForOwnLeak: re-requesting a model whose own
// stop force-killed must not start it while its old container may still be
// tearing down, even though the budget would fit it. It queues (its leak is
// pending) and starts once the leak watcher reports the upstream gone.
func TestFIFO_Memory_LeakedTargetWaitsForOwnLeak(t *testing.T) {
	models := map[string]config.ModelConfig{"a": {MemoryCeiling: 30}}
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0) // 100: fits even charged twice
	eff.states["a"] = process.StateReady
	eff.forced["a"] = true
	s.OnUnload([]string{"a"}, time.Second)

	r := reqCh("a")
	s.OnRequest(r)
	assertAdmitted(t, r)
	if eff.startsFor("a") != 0 || len(s.queued) != 1 {
		t.Fatalf("startsFor(a)=%d queued=%d want 0/1 (a waits for its own leak)", eff.startsFor("a"), len(s.queued))
	}
	if !strings.Contains(s.memoryHolders("a", nil, nil), "a=30 (itself force-killed") {
		t.Errorf("memoryHolders=%q want the target's own leak named", s.memoryHolders("a", nil, nil))
	}

	s.OnLeakGone("a", "test")
	if eff.startsFor("a") != 1 || len(s.queued) != 0 {
		t.Fatalf("startsFor(a)=%d queued=%d want 1/0 once a's old upstream is gone", eff.startsFor("a"), len(s.queued))
	}
}

// TestFIFO_Memory_UnloadMidEvictionKeepsSwapCharged: unloading b while its swap
// is still evicting a (the UI's "Cancel load") releases b's waiter and cancels
// the swap, but keeps its entry until SwapDone. Deleting it let c in on the
// budget b and a still held while the orphaned swap went on to boot b. A
// request for b in that window queues rather than joining the dead swap, and
// is started afresh once the cancelled swap reports.
func TestFIFO_Memory_UnloadMidEvictionKeepsSwapCharged(t *testing.T) {
	models := map[string]config.ModelConfig{
		"a": {MemoryCeiling: 60},
		"b": {MemoryCeiling: 30},
		"c": {MemoryCeiling: 50},
	}
	s, eff := newFIFOMem(t, &stubPlanner{evict: map[string][]string{"b": {"a"}}}, models, 100, 0)
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	eff.states["c"] = process.StateStopped

	s.OnRequest(reqCh("b"))
	eff.states["a"] = process.StateStopping // b's swap is evicting a
	s.OnUnload([]string{"b"}, time.Second)

	if eff.errored("b") != 1 {
		t.Fatalf("errored(b)=%d want 1 (the waiter is released at once)", eff.errored("b"))
	}
	if eff.starts[0].ctx.Err() == nil {
		t.Fatal("b's swap ctx not cancelled; doSwap would go on to start b")
	}
	if sw, ok := s.active["b"]; !ok || !sw.cancelled {
		t.Fatalf("active[b]=%+v want kept and marked cancelled until its SwapDone", sw)
	}

	eff.states["a"] = process.StateStopped // eviction finished; SwapDone not yet seen
	rc := reqCh("c")
	s.OnRequest(rc)
	assertAdmitted(t, rc)
	if eff.startsFor("c") != 0 || len(s.queued) != 1 {
		t.Fatalf("startsFor(c)=%d queued=%d want 0/1 (b and its evictee a stay charged: 30+60+50 > 100)", eff.startsFor("c"), len(s.queued))
	}
	rb := reqCh("b")
	s.OnRequest(rb)
	assertAdmitted(t, rb)
	if eff.startsFor("b") != 1 || len(s.queued) != 2 || len(s.active["b"].waiters) != 0 {
		t.Fatalf("startsFor(b)=%d queued=%d waiters=%d want 1/2/0 (b queues, not joining its cancelled swap)", eff.startsFor("b"), len(s.queued), len(s.active["b"].waiters))
	}

	s.OnSwapDone(SwapDone{ModelID: "b", Err: errors.New("swap to b cancelled: model unloaded")})
	if len(eff.stops) != 1 {
		t.Errorf("stops=%+v want only the unload's (a cancelled swap that did not start needs no extra stop)", eff.stops)
	}
	if eff.startsFor("c") != 1 || eff.startsFor("b") != 2 || len(s.queued) != 0 {
		t.Fatalf("startsFor(c)=%d startsFor(b)=%d queued=%d want 1/2/0 once the cancelled swap reported", eff.startsFor("c"), eff.startsFor("b"), len(s.queued))
	}
	if eff.errored("b") != 1 {
		t.Errorf("errored(b)=%d want 1 (the new request for b is served normally)", eff.errored("b"))
	}
}

// TestFIFO_CancelledSwapThatStartedAnywayIsStopped: the unload's cancel can
// land after doSwap's last check but before its start request reaches the
// process, so the unload's Stop found nothing and the target booted. Its
// successful SwapDone must stop it (with the unload's timeout), record a forced
// stop as a leak, and not clear any leak.
func TestFIFO_CancelledSwapThatStartedAnywayIsStopped(t *testing.T) {
	models := map[string]config.ModelConfig{"b": {MemoryCeiling: 30}}
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["b"] = process.StateStopped

	s.OnRequest(reqCh("b"))
	s.OnUnload([]string{"b"}, 3*time.Second) // b not started yet: nothing to stop
	eff.states["b"] = process.StateReady     // ...then it started anyway
	eff.forced["b"] = true

	s.OnSwapDone(SwapDone{ModelID: "b"})
	if len(eff.stops) != 2 || eff.stops[1].ids[0] != "b" || eff.stops[1].timeout != 3*time.Second {
		t.Fatalf("stops=%+v want a second stop of b with the unload's 3s timeout", eff.stops)
	}
	if _, ok := s.leaked["b"]; !ok {
		t.Fatalf("leaked=%v want b recorded (its stop was forced)", s.leaked)
	}
	if eff.served("b") != 0 || eff.errored("b") != 1 {
		t.Errorf("served(b)=%d errored(b)=%d want 0/1 (only the unload's release)", eff.served("b"), eff.errored("b"))
	}
	if _, ok := s.active["b"]; ok {
		t.Error("cancelled swap entry not removed on its SwapDone")
	}
}

// TestFIFO_Memory_SelfStopDrainsQueue: c queued because a was stopping on its
// own (a TTL unload; memoryPending counts StateStopping). Nothing the router
// did started that stop, so only the self-stop report can re-check c once a
// is gone.
func TestFIFO_Memory_SelfStopDrainsQueue(t *testing.T) {
	models := map[string]config.ModelConfig{
		"a": {MemoryCeiling: 60},
		"c": {MemoryCeiling: 60},
	}
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["a"] = process.StateStopping // TTL unload in progress
	eff.states["c"] = process.StateStopped

	r := reqCh("c")
	s.OnRequest(r)
	assertAdmitted(t, r)
	if eff.startsFor("c") != 0 || len(s.queued) != 1 {
		t.Fatalf("startsFor(c)=%d queued=%d want 0/1 while a is stopping", eff.startsFor("c"), len(s.queued))
	}

	eff.states["a"] = process.StateStopped
	s.OnSelfStop("a", false)
	if eff.startsFor("c") != 1 || len(s.queued) != 0 {
		t.Fatalf("startsFor(c)=%d queued=%d want 1/0 after a's TTL stop finished", eff.startsFor("c"), len(s.queued))
	}
	if len(s.leaked) != 0 {
		t.Errorf("leaked=%v want none for a graceful stop", s.leaked)
	}
}

// TestFIFO_Memory_ForcedSelfStopRecordsLeak: a TTL stop that force-killed is a
// leak like a forced unload; the queued load keeps waiting until the watcher
// reports a's upstream gone.
func TestFIFO_Memory_ForcedSelfStopRecordsLeak(t *testing.T) {
	models := map[string]config.ModelConfig{
		"a": {MemoryCeiling: 60},
		"c": {MemoryCeiling: 60},
	}
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["a"] = process.StateStopping
	eff.states["c"] = process.StateStopped
	s.OnRequest(reqCh("c"))

	eff.states["a"] = process.StateStopped
	s.OnSelfStop("a", true)
	if _, ok := s.leaked["a"]; !ok || !eff.watching["a"] {
		t.Fatalf("leaked=%v watching=%v want a leaked and watched", s.leaked, eff.watching)
	}
	if eff.startsFor("c") != 0 || len(s.queued) != 1 {
		t.Fatalf("startsFor(c)=%d queued=%d want 0/1 while a's leak is live", eff.startsFor("c"), len(s.queued))
	}
	s.OnLeakGone("a", "test")
	if eff.startsFor("c") != 1 {
		t.Fatalf("startsFor(c)=%d want 1 after the leak cleared", eff.startsFor("c"))
	}
}

// TestFIFO_Memory_LateForcedSelfStopIgnoredOnceRestarted: the self-stop report
// is asynchronous. If the model is already being started again by the time it
// arrives, its footprint is counted through the running set, so recording a
// leak would double-count it.
func TestFIFO_Memory_LateForcedSelfStopIgnoredOnceRestarted(t *testing.T) {
	models := map[string]config.ModelConfig{"a": {MemoryCeiling: 60}}
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["a"] = process.StateStopped
	s.OnRequest(reqCh("a")) // swap for a in flight
	s.OnSelfStop("a", true)
	if len(s.leaked) != 0 {
		t.Fatalf("leaked=%v want none: a is being started again", s.leaked)
	}
}

// unwatchedLeakFIFO leaves a (60) leaked with no watch (WatchLeak returned
// false: no runningCheck, never seen healthy) on a 100 pool, so c (60) cannot
// fit beside it and nothing in progress could free it.
func unwatchedLeakFIFO(t *testing.T) (*FIFO, *fakeEffects) {
	t.Helper()
	models := map[string]config.ModelConfig{
		"a": {MemoryCeiling: 60},
		"c": {MemoryCeiling: 60},
	}
	s, eff := newFIFOMem(t, &stubPlanner{}, models, 100, 0)
	eff.states["a"] = process.StateStarting // killed while still loading
	eff.states["c"] = process.StateStopped
	eff.forced["a"] = true
	eff.unwatchable["a"] = true
	s.OnUnload([]string{"a"}, time.Second)
	delete(eff.forced, "a")
	if watching, ok := s.leaked["a"]; !ok || watching || eff.watching["a"] {
		t.Fatalf("leaked=%v watching=%v want a leaked and unwatched", s.leaked, eff.watching)
	}
	return s, eff
}

// TestFIFO_Memory_UnwatchedLeakRefusesNamingIt: a leak nothing can report gone
// is not pending, so a load blocked by it gets the 503 at once instead of
// queuing until its client gives up, and the message tells the user which
// model to unload. The same holds for a request for the leaked model itself.
func TestFIFO_Memory_UnwatchedLeakRefusesNamingIt(t *testing.T) {
	s, eff := unwatchedLeakFIFO(t)

	for _, model := range []string{"c", "a"} {
		r := reqCh(model)
		s.OnRequest(r)
		err := admitErr(t, r)
		var memErr swaputil.MemoryAdmissionError
		if !errors.As(err, &memErr) {
			t.Fatalf("%s: admission err=%v want MemoryAdmissionError", model, err)
		}
		for _, want := range []string{"a=60 (", "leaked: force-killed, still running?", "unload a to release"} {
			if !strings.Contains(memErr.Message, want) {
				t.Errorf("%s: refusal message %q missing %q", model, memErr.Message, want)
			}
		}
	}
	if len(s.queued) != 0 || eff.startsFor("c") != 0 || eff.startsFor("a") != 0 {
		t.Fatalf("queued=%d starts c/a=%d/%d want nothing queued or started", len(s.queued), eff.startsFor("c"), eff.startsFor("a"))
	}
}

// TestFIFO_Memory_UnloadRerunsCmdStopOfStoppedLeak: Stop on an already-stopped
// process is a no-op, so an unload of a stopped leaked model runs its cmdStop
// again (Effects.RerunStop) and clears the leak only when that completed.
func TestFIFO_Memory_UnloadRerunsCmdStopOfStoppedLeak(t *testing.T) {
	s, eff := unwatchedLeakFIFO(t)

	s.OnUnload([]string{"a", "c"}, time.Second) // cmdStop fails
	if !slices.Equal(eff.reruns, []string{"a"}) {
		t.Fatalf("reruns=%v want [a] (c is not leaked)", eff.reruns)
	}
	if _, ok := s.leaked["a"]; !ok {
		t.Fatal("leak cleared although its cmdStop did not complete")
	}

	eff.rerunOK["a"] = true
	s.OnUnload([]string{"a"}, time.Second)
	if _, ok := s.leaked["a"]; ok {
		t.Fatal("leak not cleared after its cmdStop completed")
	}

	r := reqCh("c")
	s.OnRequest(r)
	assertAdmitted(t, r)
	if eff.startsFor("c") != 1 {
		t.Fatalf("startsFor(c)=%d want 1 once a is released", eff.startsFor("c"))
	}
}

// TestFIFO_Memory_UnwatchedLeakClearedWhenStartedAgain: the existing
// "started again" rule also releases an unwatched leak.
func TestFIFO_Memory_UnwatchedLeakClearedWhenStartedAgain(t *testing.T) {
	s, eff := unwatchedLeakFIFO(t)
	s.OnRequest(adoptReq("a"))
	if eff.startsFor("a") != 1 {
		t.Fatalf("startsFor(a)=%d want 1 (adopt bypasses the gate)", eff.startsFor("a"))
	}
	eff.states["a"] = process.StateReady
	s.OnSwapDone(SwapDone{ModelID: "a"})
	if _, ok := s.leaked["a"]; ok {
		t.Fatalf("leaked=%v want a cleared after a successful start", s.leaked)
	}
}
