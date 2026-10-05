package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// defaultConcurrencyLimit caps simultaneous in-flight requests per model when
// the model config leaves concurrencyLimit unset.
const defaultConcurrencyLimit = 10

// activeSwap tracks one in-flight swap and the callers waiting on it.
type activeSwap struct {
	modelID string
	evict   []string
	waiters []HandlerReq
	// cancel cancels the ctx handed to Effects.StartSwap.
	cancel context.CancelFunc
	// cancelled marks a swap whose target was unloaded mid-swap. Its waiters are
	// already released, but the entry stays in active until its SwapDone,
	// because its goroutine may still be stopping evictees (or, if the cancel
	// lost the race with EnsureReady, starting the target): until then the
	// target and its evictees stay charged and colliding requests keep
	// queuing. New requests for the target queue instead of joining it.
	cancelled bool
	// unloadTimeout is the cancelling unload's stop timeout, reused if the
	// target turns out to have started anyway (see OnSwapDone).
	unloadTimeout time.Duration
}

// FIFO is the default scheduler. Requests are handled in a first-in, first-out order.
// To reduce swapping requests for a model that is already running will be handled
// immediately by the running process.
//
// Requests into this schedule are handled like this:
//
// A B C A B C --> A A B B C C
//
// The strategy is simple and reduces the number of swaps required.
type FIFO struct {
	name    string
	logger  *logmon.Monitor
	planner Swapper
	cfg     config.FifoConfig
	effects Effects

	limits   map[string]int
	ceilings map[string]int64 // model ID -> hard resident footprint in bytes (0 = unset)
	pool     int64            // usable unified-memory budget in bytes (0 disables the gate)
	reserve  int64            // headroom kept free below pool
	active   map[string]*activeSwap
	reserved map[string]int
	inFlight map[string]int
	queued   []HandlerReq
	// leaked holds models whose stop force-killed (process.ErrForcedKill, from a
	// SwapDone, a StopProcesses, or a report taken by recordForcedStops): the
	// process reports stopped but its upstream may still hold memory, so fits
	// keeps charging their ceilings until the leak watcher reports the upstream
	// gone (OnLeakGone), the model is started again (OnSwapDone), or an unload
	// re-runs its cmdStop successfully (OnUnload). The value is whether the
	// leak counts as pending (memoryPending): true while a watcher is running
	// (Effects.WatchLeak) and has not reported it stale (OnLeakStale). Only
	// such a leak is expected to clear on its own soon; a stale one keeps its
	// watcher, and may still clear, but loads it blocks are refused.
	leaked map[string]bool
}

// NewFIFO builds a FIFO scheduler. Per-model concurrency limits and memory
// ceilings are derived from models. pool/reserve are the box-wide memory budget:
// pool == 0 disables the memory-admission gate entirely.
func NewFIFO(name string, logger *logmon.Monitor, planner Swapper, cfg config.FifoConfig, models map[string]config.ModelConfig, pool, reserve int64, eff Effects) *FIFO {
	limits := make(map[string]int, len(models))
	ceilings := make(map[string]int64, len(models))
	for id, mc := range models {
		limit := defaultConcurrencyLimit
		if mc.ConcurrencyLimit > 0 {
			limit = mc.ConcurrencyLimit
		}
		limits[id] = limit
		ceilings[id] = mc.MemoryCeiling
	}

	// Surface unsized models once at startup when the gate is active: a new load
	// of one is refused (can't be sized) and, if it becomes resident via
	// adopt/reload, it's under-counted. Better a loud line than a silent gap.
	if pool > 0 {
		var unsized []string
		for id, c := range ceilings {
			if c <= 0 {
				unsized = append(unsized, id)
			}
		}
		if len(unsized) > 0 {
			sort.Strings(unsized)
			logger.Warnf("memory admission: %d model(s) have no memoryCeiling and will be refused as new loads / under-counted if resident: %v", len(unsized), unsized)
		}
	}

	return &FIFO{
		name:     name,
		logger:   logger,
		planner:  planner,
		cfg:      cfg,
		effects:  eff,
		limits:   limits,
		ceilings: ceilings,
		pool:     pool,
		reserve:  reserve,
		active:   make(map[string]*activeSwap),
		reserved: make(map[string]int),
		inFlight: make(map[string]int),
		leaked:   make(map[string]bool),
	}
}

// OnRequest decides what to do with one incoming ServeHTTP request. It never
// blocks indefinitely: any work that has to wait (starting a process, stopping
// siblings, waiting for ready) is deferred to a swap goroutine and reported back
// via OnSwapDone.
//
// Three refusals are answered on the ADMISSION channel rather than after
// admission: an unknown model, a new load the memory budget can never fit, and
// a new load that does not fit now while nothing in progress could free memory
// for it (step (1.5) in the body). That seam is before the caller can start a
// loading stream, which is what keeps them clean status codes.
//
// The decision tree, in order:
//
//  1. Unknown model — respond with ErrModelNotFound and move on.
//  2. A swap to the same model is already in flight — attach this waiter so
//     one swap serves all callers that asked for the same model. A swap an
//     unload cancelled is not joined: the request queues until its SwapDone.
//     With the memory gate on, a target that is still stopping also queues,
//     until that stop's outcome (forced or not) is known.
//  3. Fast path — the target process is already ready, the planner sees
//     nothing to evict, and no in-flight swap is evicting it. Hand back its
//     ServeHTTP immediately.
//  4. Would collide with an in-flight swap (we'd stop their target, or they're
//     stopping us) — park in the queue for OnSwapDone to drain.
//  5. Would evict a process that is still handling requests — park in the
//     queue. OnServeDone will retry when the busy process drains.
//  6. Otherwise — start a new swap. This may run in parallel with other active
//     swaps when their evict sets don't intersect.
func (s *FIFO) OnRequest(req HandlerReq) {
	// A forced TTL stop that finished but whose OnSelfStop has not arrived yet
	// must be charged before anything below credits that model as freed.
	s.recordForcedStops()

	// (1) Unknown model.
	state, ok := s.effects.ModelState(req.Model)
	if !ok {
		s.logger.Debugf("%s: model %s not handled by this router", s.name, req.Model)
		s.rejectAdmission(req, ErrModelNotFound)
		return
	}

	// The eviction picture is built BEFORE admission because the hard memory
	// refusal below needs it. A request joining an in-flight swap for the same
	// model does not: that swap's memory was already admitted.
	sw, inSwap := s.active[req.Model]
	joining := inSwap && !sw.cancelled
	// A cancelled swap for this model is still winding down; the request waits
	// for its SwapDone in the queue (drainQueue then decides it afresh).
	behindCancelled := inSwap && sw.cancelled
	// The target is still stopping (a TTL unload, or an eviction by another
	// swap). Whether that stop force-kills is only known once it finishes, and
	// starting now would have EnsureReady start the model the moment the stop
	// returns, its start command (`compose up -d`) racing a forced stop's
	// teardown of the same container. So the request waits for the event that
	// ends every such stop and drains the queue (OnSelfStop, OnSwapDone), which
	// then sees a forced stop as a leak of the target. Only with the memory
	// gate on, because only it tracks forced stops; without it EnsureReady's
	// wait-then-start is kept. This state read only defers: every stop ends in
	// one of those draining events, so unlike issue #946 nothing is left
	// waiting on a start nobody will make.
	behindOwnStop := s.pool > 0 && !inSwap && state == process.StateStopping
	var (
		running, evict []string
		fastPath       bool
	)
	if !inSwap {
		running = s.runningSet(req.Model)
		evict = s.planner.EvictionFor(req.Model, running)
		// (3) Fast path: ready, nothing to evict, and nobody is evicting us.
		fastPath = state == process.StateReady && len(evict) == 0 && !collidesWith(req.Model, evict, s.active)
	}

	// (1.5) Memory refusals, decided BEFORE admission succeeds. Answering on the
	// admission channel is what keeps them a clean 503: past admit(), a
	// streaming caller has already been handed 200 + SSE headers by the loading
	// writer, and the refusal can then only be framed into the stream (#1029's
	// failure-reported-as-success shape). This is the same pre-stream seam the
	// concurrency-limit rejection uses. Two cases refuse here:
	//   - never fits: the model's own ceiling exceeds the budget (or is
	//     unknown), so no eviction and no waiting can ever help.
	//   - does not fit now and nothing is pending (memoryPending): the models
	//     holding the budget are ones the planner will not evict, and nothing
	//     in motion could free memory, so queuing would wait forever (no event
	//     would ever drain it).
	// A load that does not fit but has something pending is admitted and
	// queued below, waiting for that to settle.
	memFits := joining || behindCancelled || behindOwnStop || fastPath || isAdopt(req) || s.fits(req.Model, evict, running)
	if !memFits {
		if s.neverFits(req.Model) {
			s.logger.Debugf("%s: refusing model %s (never fits memory budget)", s.name, req.Model)
			s.rejectAdmission(req, swaputil.MemoryAdmissionError{Message: s.memoryRejectMessage(req.Model)})
			return
		}
		if !s.memoryPending() {
			msg := s.memoryBlockedMessage(req.Model, evict, running)
			s.logger.Warnf("%s: refusing model %s: %s", s.name, req.Model, msg)
			s.rejectAdmission(req, swaputil.MemoryAdmissionError{Message: msg})
			return
		}
	}

	if !s.admit(req) {
		return
	}

	// (2) Join an in-flight swap for the same model.
	if joining {
		s.logger.Debugf("%s: joining in-flight swap for model %s (%d waiters)", s.name, req.Model, len(sw.waiters)+1)
		sw.waiters = append(sw.waiters, req)
		return
	}

	if behindCancelled {
		s.logger.Debugf("%s: queuing request for model %s (its cancelled swap has not finished)", s.name, req.Model)
		s.enqueue(req)
		return
	}

	if behindOwnStop {
		s.logger.Debugf("%s: queuing request for model %s (it is still stopping)", s.name, req.Model)
		s.enqueue(req)
		return
	}

	// (3) Fast path.
	if fastPath {
		s.logger.Debugf("%s: fast-path serving model %s (already ready)", s.name, req.Model)
		s.grantHandler(req, req.Model)
		return
	}

	// Memory admission: a NEW load must fit the pool once the evict set frees.
	// Only the pending case reaches here (both refusals were answered before
	// admission), so it waits for the in-progress work to free memory.
	if !memFits {
		// Warn, not Debug, because a load that can't fit beside residents the
		// planner won't evict otherwise waits with no visible trace.
		s.logger.Warnf("%s: queuing model %s (does not fit now; waiting for memory to free): needs %d bytes, budget %d (pool %d - reserve %d), still resident after eviction: %s",
			s.name, req.Model, s.ceilings[req.Model], s.pool-s.reserve, s.pool, s.reserve, s.memoryHolders(req.Model, evict, running))
		s.enqueue(req)
		return
	}

	// (4) Collision with an in-flight swap — queue.
	if collidesWith(req.Model, evict, s.active) {
		s.logger.Debugf("%s: queuing request for model %s (collides with in-flight swap)", s.name, req.Model)
		s.enqueue(req)
		return
	}

	// (5) Would evict a busy process — queue until it drains.
	if conflictsWithInFlight(evict, s.inFlight) {
		s.logger.Debugf("%s: queuing request for model %s (would evict in-flight process)", s.name, req.Model)
		s.enqueue(req)
		return
	}

	// (6) Start a new (possibly parallel) swap.
	s.logger.Debugf("%s: starting swap for model %s, evicting %v", s.name, req.Model, evict)
	s.startSwap(req, evict, running)
}

// OnCancel removes a request whose client has disconnected from the queue and
// from every in-flight swap's waiters. If the request was the sole waiter of an
// active swap, the swap goroutine is left to complete on its own — OnSwapDone
// will find no waiters and simply clean up. This prevents drainQueue from ever
// starting a model load for a caller that is no longer there.
func (s *FIFO) OnCancel(req HandlerReq) {
	removed := false

	// Prune from the queue.
	if len(s.queued) > 0 {
		kept := s.queued[:0]
		for _, q := range s.queued {
			if q.Respond == req.Respond {
				removed = true
				s.release(q.Model)
				continue
			}
			kept = append(kept, q)
		}
		s.queued = kept
	}

	// Prune from any active swap's waiters.
	for _, sw := range s.active {
		filtered := sw.waiters[:0]
		for _, w := range sw.waiters {
			if w.Respond == req.Respond {
				removed = true
				s.release(w.Model)
				continue
			}
			filtered = append(filtered, w)
		}
		sw.waiters = filtered
	}

	if removed {
		s.logger.Debugf("%s: cancelled request for model %s pruned from scheduler", s.name, req.Model)
		broadcastQueuePositions(s.queued)
	}
}

// OnSwapDone fans the result out to every waiter that joined this swap, removes
// the swap from the active map, then walks the queue once, promoting any items
// that no longer collide with the remaining active set. FIFO order is preserved:
// items still blocked stay in place.
func (s *FIFO) OnSwapDone(ev SwapDone) {
	// Record leaks before the early return: a swap whose waiters OnUnload
	// already released still force-killed what it force-killed.
	s.recordLeaks(ev.Leaked)

	sw, ok := s.active[ev.ModelID]
	if ok && !sw.cancelled && ev.Err == nil {
		// Started again: its footprint is now counted as resident through the
		// running set, so the leak entry would double-count it. Both guards are
		// needed because a successful EnsureReady can be overtaken before its
		// SwapDone arrives here. Owning a live (not cancelled) entry rules out an
		// OnUnload that already force-killed the target (it cancelled the entry
		// and recorded the leak this stale success must not erase); the model still
		// reading Ready rules out it having stopped on its own (TTL, crash) in
		// that gap. Either alone leaves one of those paths able to clear a leak
		// whose container may still be up.
		if st, _ := s.effects.ModelState(ev.ModelID); st == process.StateReady {
			s.clearLeak(ev.ModelID, "started again")
		}
	}
	if !ok {
		return
	}
	delete(s.active, ev.ModelID)
	sw.cancel()

	if sw.cancelled {
		// Its waiters were released by OnUnload. A success here means the
		// target started although the unload cancelled the swap: the cancel
		// landed after doSwap's last check but before its start request reached
		// the process, so the unload's Stop found nothing to stop. Stop it now,
		// as the unload asked, before its memory is credited as free.
		if ev.Err == nil {
			s.logger.Warnf("%s: model %s started after its swap was cancelled by an unload; stopping it", s.name, ev.ModelID)
			s.recordLeaks(s.effects.StopProcesses(sw.unloadTimeout, []string{ev.ModelID}))
		}
		s.drainQueue()
		return
	}

	for _, w := range sw.waiters {
		if ev.Err != nil {
			s.grantError(w, ev.Err)
		} else {
			s.grantHandler(w, ev.ModelID)
		}
	}

	s.drainQueue()
}

// OnServeDone decrements the per-model in-flight count and, when that drops to
// zero, retries the queue: requests whose swap was deferred because they would
// have evicted this (now-idle) process can now proceed.
func (s *FIFO) OnServeDone(ev ServeDoneEvent) {
	s.inFlight[ev.ModelID]--
	s.release(ev.ModelID)
	if s.inFlight[ev.ModelID] <= 0 {
		delete(s.inFlight, ev.ModelID)
		s.drainQueue()
	}
}

// OnUnload reconciles router-owned state with the impending Stop, performs the
// Stop (synchronously, via Effects) so callers of Unload remain blocked until
// each targeted process has exited, then drains the queue.
func (s *FIFO) OnUnload(targets []string, timeout time.Duration) {
	unloadErr := fmt.Errorf("%s: model unloaded", s.name)
	// A target already stopped by an unreported forced stop becomes a leak
	// here, so the rerun pass below releases it like any other stopped leak.
	s.recordForcedStops()

	targetSet := make(map[string]bool, len(targets))
	for _, id := range targets {
		targetSet[id] = true
	}

	// Cancel any in-flight swap whose target is being unloaded and release its
	// waiters. The entry is kept (see activeSwap.cancelled) so the target and
	// its evictees stay charged until the swap's SwapDone; deleting it here let
	// a second load in while the orphaned swap went on to start its target.
	// Cancelling the swap's ctx makes doSwap skip that start.
	for id := range targetSet {
		sw, ok := s.active[id]
		if !ok {
			continue
		}
		for _, w := range sw.waiters {
			s.grantError(w, unloadErr)
		}
		sw.waiters = nil
		sw.cancelled = true
		sw.unloadTimeout = timeout
		sw.cancel()
	}

	// Drop queued requests addressed to unloaded models. Requests for other
	// models stay queued and may benefit from drainQueue at the end.
	if len(s.queued) > 0 {
		kept := s.queued[:0]
		for _, w := range s.queued {
			if targetSet[w.Model] {
				s.grantError(w, unloadErr)
				continue
			}
			kept = append(kept, w)
		}
		s.queued = kept
	}

	// A leaked target that already reads Stopped (and no swap owns) has no
	// process left for Stop to act on, so Stop would be a no-op and the leak
	// could only wait for its watcher. The owner unloading it is the explicit
	// "release it" signal, so its cmdStop runs again below (after the Stop
	// pass, which leaves such a process untouched).
	var rerun []string
	for _, id := range targets {
		if _, leaked := s.leaked[id]; !leaked {
			continue
		}
		if _, inSwap := s.active[id]; inSwap {
			continue
		}
		if st, _ := s.effects.ModelState(id); st == process.StateStopped {
			rerun = append(rerun, id)
		}
	}

	// Stop the targeted processes. Done synchronously so Unload's caller can
	// rely on "after Unload returns, the process is stopped". inFlight is
	// intentionally NOT cleared here: each dying handler will fire its tracked
	// serve and reach OnServeDone in the normal way. A stop that force-killed
	// is recorded as a leak so its memory is not credited as freed.
	s.recordLeaks(s.effects.StopProcesses(timeout, targets))

	if len(rerun) > 0 {
		for _, id := range s.effects.RerunStop(timeout, rerun) {
			s.clearLeak(id, "unloaded and its cmdStop completed")
		}
	}

	// Stopping the targets and dropping their queued requests may have
	// unblocked queued requests for other models.
	s.drainQueue()
}

// OnLeakGone clears a leak once the watcher saw its upstream gone, then drains
// the queue: loads waiting on that memory may now fit.
func (s *FIFO) OnLeakGone(modelID, reason string) {
	if _, ok := s.leaked[modelID]; !ok {
		return
	}
	s.clearLeak(modelID, reason)
	s.drainQueue()
}

// OnSelfStop drains the queue after a process stopped on its own: a load that
// queued behind it (memoryPending counts a StateStopping model) may now fit, or
// must now be refused, and nothing else would re-check it. A forced TTL stop is
// recorded as a leak first, like a forced unload, by drainQueue's
// recordForcedStops, which takes the process's own forced-stop report rather
// than trusting forced: this report is asynchronous, so by now that forced stop
// may already be recorded (a request, unload or eviction in between took it),
// or the model may be starting again, when its footprint is counted through the
// running set and a leak entry would double-count it. Either way there is no
// report left to take, so nothing is recorded twice.
func (s *FIFO) OnSelfStop(modelID string, forced bool) {
	s.logger.Debugf("%s: model %s stopped on its own (forced=%t)", s.name, modelID, forced)
	s.drainQueue()
}

// recordForcedStops records a leak for every stopped model whose last stop
// force-killed and has not been accounted for yet (Effects.TakeForcedStops),
// so a forced TTL stop is charged from the moment it finished rather than from
// when its OnSelfStop arrives. Models an in-flight swap is stopping or starting
// are left to that swap's SwapDone. A no-op with the gate off.
func (s *FIFO) recordForcedStops() {
	if s.pool == 0 {
		return
	}
	var exclude []string
	for id, sw := range s.active {
		exclude = append(exclude, id)
		exclude = append(exclude, sw.evict...)
	}
	s.recordLeaks(s.effects.TakeForcedStops(exclude))
}

// OnLeakStale stops counting a watched leak as pending once its watcher has
// seen the upstream still running for longer than a forced stop's teardown
// takes. Such an upstream is most likely genuinely running (e.g. a container
// that came up after its supervisor was SIGKILLed while its cmd waited to
// start it), so loads it blocks would otherwise queue without bound. It stays
// charged, and its watcher keeps running so it can still clear, but from now
// on it is treated like an unwatched leak: a load it blocks gets the immediate
// 503 naming it to unload, and the drain below refuses loads already queued
// behind it (unless something else is still pending).
func (s *FIFO) OnLeakStale(modelID, reason string) {
	if watching, ok := s.leaked[modelID]; !ok || !watching {
		return
	}
	s.leaked[modelID] = false
	s.logger.Warnf("%s: leaked model %s is %s; no longer waiting for it: loads that need its %d-byte memoryCeiling are refused until it is unloaded (its cmdStop completes) or its runningCheck/probe sees it gone", s.name, modelID, reason, s.ceilings[modelID])
	s.drainQueue()
}

// recordLeaks marks force-killed models as still holding their ceilings and
// starts watching their upstreams. A no-op with the gate off (pool == 0),
// because there is no budget to charge them against.
func (s *FIFO) recordLeaks(ids []string) {
	if s.pool == 0 {
		return
	}
	for _, id := range ids {
		// Already counted: a forced stop can reach here twice (taken by
		// recordForcedStops, then also carried by a SwapDone), and restarting
		// the watch would add nothing.
		if _, ok := s.leaked[id]; ok {
			continue
		}
		watching := s.effects.WatchLeak(id)
		s.leaked[id] = watching
		if watching {
			s.logger.Warnf("%s: %s was force-killed; counting its %d-byte memoryCeiling as resident until its upstream is gone", s.name, id, s.ceilings[id])
		} else {
			s.logger.Warnf("%s: %s was force-killed and nothing can tell when its upstream is gone; counting its %d-byte memoryCeiling as resident until it is unloaded (cmdStop completes) or started again", s.name, id, s.ceilings[id])
		}
	}
}

// clearLeak drops a leak entry (if any) and stops watching it.
func (s *FIFO) clearLeak(modelID, why string) {
	if _, ok := s.leaked[modelID]; !ok {
		return
	}
	delete(s.leaked, modelID)
	s.effects.UnwatchLeak(modelID)
	s.logger.Infof("%s: leaked model %s cleared (%s); its memoryCeiling is no longer counted as leaked", s.name, modelID, why)
}

// OnShutdown grants err to every waiter still held by the scheduler.
func (s *FIFO) OnShutdown(err error) {
	for _, sw := range s.active {
		for _, w := range sw.waiters {
			s.grantError(w, err)
		}
	}
	for _, w := range s.queued {
		s.grantError(w, err)
	}
}

// grantHandler hands the caller a tracked handler for modelID and, only if the
// caller was still there to receive it, bumps the in-flight count. Incrementing
// when the grant failed would strand the counter and block future evictions.
// Concurrency-limit rejection happens earlier in admit, before a request can
// start the loading stream.
func (s *FIFO) grantHandler(req HandlerReq, modelID string) {
	if err := swaputil.SetReqData(req.Ctx, "fifo_priority", strconv.Itoa(s.cfg.Priority[req.Model])); err != nil {
		s.logger.Debugf("failed to set fifo_priority metadata: %v", err)
	}

	if s.effects.GrantServe(req, modelID) {
		s.inFlight[modelID]++
	} else {
		s.release(modelID)
	}
}

// grantError reports a post-admission error to the caller and releases the
// request's reserved concurrency slot.
func (s *FIFO) grantError(req HandlerReq, err error) {
	s.release(req.Model)
	s.effects.GrantError(req, err)
}

// admit performs the pre-stream admission handshake. Accepted requests reserve
// one future serving slot until they serve, cancel while waiting, or receive a
// post-admission error.
func (s *FIFO) admit(req HandlerReq) bool {
	if s.reserved[req.Model] >= s.limit(req.Model) {
		s.rejectAdmission(req, swaputil.ConcurrencyLimitError{})
		return false
	}
	if !sendAdmission(req, nil) {
		return false
	}
	s.reserved[req.Model]++
	return true
}

func (s *FIFO) rejectAdmission(req HandlerReq, err error) {
	sendAdmission(req, err)
}

func sendAdmission(req HandlerReq, err error) bool {
	if req.Admit == nil {
		return true
	}
	done := reqDone(req)
	select {
	case <-done:
		return false
	default:
	}
	select {
	case req.Admit <- err:
		return true
	case <-done:
		return false
	}
}

func reqDone(req HandlerReq) <-chan struct{} {
	if req.Ctx == nil {
		return nil
	}
	return req.Ctx.Done()
}

func (s *FIFO) release(modelID string) {
	if s.reserved[modelID] <= 0 {
		panic(fmt.Sprintf("%s: release without reservation for model %s", s.name, modelID))
	}
	s.reserved[modelID]--
	if s.reserved[modelID] == 0 {
		delete(s.reserved, modelID)
	}
}

// limit returns the per-model concurrency cap, defaulting to
// defaultConcurrencyLimit when the model has no explicit entry.
func (s *FIFO) limit(modelID string) int {
	if l, ok := s.limits[modelID]; ok {
		return l
	}
	return defaultConcurrencyLimit
}

// fits reports whether target can be admitted under the memory budget once the
// planned evict set is stopped. pool == 0 disables the gate (feature off). A
// target with no configured ceiling can't be sized, so it never fits (the caller
// treats that as a hard refuse via neverFits). A leaked target does not fit
// until its leak clears (see the check below). The charged set is memoryCharged:
// running (the pre-swap resident + in-flight set) minus evict and target, plus
// every leaked model. running includes target whenever target's process is not
// stopped (Starting, Ready, Stopping); it is skipped so target is counted once,
// as needed. A RESIDENT model with no ceiling is a config error: counted as 0
// (best-effort) rather than deadlocking the queue — the shipped earlyoom +
// fail-closed compose gate backstop an actual OOM.
func (s *FIFO) fits(target string, evict, running []string) bool {
	if s.pool == 0 {
		return true
	}
	if s.ceilings[target] <= 0 {
		return false
	}
	// A target that is itself leaked waits for its leak to clear, whatever the
	// budget says: starting it now would run its start command (`compose up -d`)
	// against its own force-killed container while that is still being torn
	// down. A watched leak counts as pending, so the request queues until
	// OnLeakGone; an unwatched one is refused, naming the model to unload.
	if _, ok := s.leaked[target]; ok {
		return false
	}
	budget := s.pool - s.reserve
	needed := s.ceilings[target]
	if needed > budget {
		return false
	}
	for _, id := range s.memoryCharged(target, evict, running) {
		// A resident model with no configured ceiling is a config error (flagged
		// once at startup in NewFIFO). Count it as 0 (best-effort) rather than
		// deadlocking the queue; earlyoom + the fail-closed compose gate backstop
		// an actual OOM.
		needed += s.ceilings[id]
		if needed < 0 || needed > budget { // needed<0 catches int64 overflow
			return false
		}
	}
	return true
}

// memoryCharged is the set fits charges against the budget besides target:
// running minus evict and target, then (sorted, not already listed) every
// leaked model and every evictee of another in-flight swap.
//   - A leaked model is charged even when it is in evict, because stopping it
//     again does not free what a force-killed upstream still holds; only the
//     leak watcher (or a fresh start) settles it.
//   - An in-flight swap's evictees are charged until that swap reports done,
//     even once their process reads Stopped, because whether the stop was
//     forced (a leak) only arrives with its SwapDone; crediting them earlier
//     would let this load in on top of a still-resident container.
func (s *FIFO) memoryCharged(target string, evict, running []string) []string {
	var out []string
	seen := map[string]struct{}{target: {}}
	for _, id := range running {
		if _, dup := seen[id]; dup || containsString(evict, id) {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	var extra []string
	add := func(id string) {
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		extra = append(extra, id)
	}
	for id := range s.leaked {
		add(id)
	}
	for _, sw := range s.active {
		for _, id := range sw.evict {
			add(id)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// memoryHolders lists memoryCharged with ceilings in bytes (leaked models
// marked), for the memory-queue log line and the memory-refusal message.
func (s *FIFO) memoryHolders(target string, evict, running []string) string {
	var holders []string
	if _, ok := s.leaked[target]; ok {
		holders = append(holders, fmt.Sprintf("%s=%d (itself %s)", target, s.ceilings[target], s.leakNote(target)))
	}
	for _, id := range s.memoryCharged(target, evict, running) {
		h := fmt.Sprintf("%s=%d", id, s.ceilings[id])
		if _, ok := s.leaked[id]; ok {
			h += " (" + s.leakNote(id) + ")"
		}
		holders = append(holders, h)
	}
	if len(holders) == 0 {
		return "none"
	}
	return strings.Join(holders, ", ")
}

// leakNote describes a leaked model for memoryHolders. An unwatched leak says
// it will not clear by itself, because only an unload (or a start) releases it.
func (s *FIFO) leakNote(id string) string {
	if s.leaked[id] {
		return "force-killed, may still hold memory"
	}
	return "leaked: force-killed, still running?"
}

// memoryPending reports whether anything already in motion could still free
// memory for a load that does not fit now: an in-flight swap (its evictions are
// underway, and a failed start releases its target's share), a model in
// StateStopping (a stop that has not finished), or a watched leaked model (its
// upstream is being watched and has not been reported stale). Only then is it
// worth queuing an over-budget load, because each ends in an event that runs
// drainQueue again (OnSwapDone, OnUnload or OnSelfStop for a TTL stop,
// OnLeakGone or OnLeakStale). An unwatched or stale leak is not pending:
// nothing is expected to report it gone, so a load waiting on it would queue
// until its client gave up; it is refused instead, naming the model to unload
// (memoryBlockedMessage).
// The rule is deliberately coarse (it does not ask whether the pending work
// would free enough): drainQueue re-checks and refuses once nothing is pending,
// so the cost of a false "pending" is a bounded wait, not a stranded request.
func (s *FIFO) memoryPending() bool {
	if len(s.active) > 0 {
		return true
	}
	for _, watching := range s.leaked {
		if watching {
			return true
		}
	}
	for _, st := range s.effects.RunningModels() {
		if st == process.StateStopping {
			return true
		}
	}
	return false
}

// memoryBlockedMessage builds the client-facing 503 message for a load that
// does not fit now and has nothing pending that could free memory for it. When
// an unwatched leak is among the holders (or is the target itself), it tells
// the user to unload that model, because nothing else will release it.
func (s *FIFO) memoryBlockedMessage(target string, evict, running []string) string {
	msg := fmt.Sprintf("model %q needs %d bytes but the budget of %d (pool %d - reserve %d) is held by models that will not be evicted for it: %s",
		target, s.ceilings[target], s.pool-s.reserve, s.pool, s.reserve, s.memoryHolders(target, evict, running))
	var stuck []string
	if watching, ok := s.leaked[target]; ok && !watching {
		stuck = append(stuck, target)
	}
	for _, id := range s.memoryCharged(target, evict, running) {
		if watching, ok := s.leaked[id]; ok && !watching {
			stuck = append(stuck, id)
		}
	}
	if len(stuck) > 0 {
		msg += fmt.Sprintf(" - unload %s to release", strings.Join(stuck, ", "))
	}
	return msg
}

// isAdopt reports whether req is an adoption attach (StartAdopt sets the "adopt"
// metadata key). Adopt attaches to an already-running container and spends no
// new memory, so it must skip the memory-admission gate — gating it would refuse
// a live model and leave it invisible to the ledger.
func isAdopt(req HandlerReq) bool {
	if req.Ctx == nil {
		return false
	}
	data, ok := swaputil.ReadContext(req.Ctx)
	return ok && data.Metadata["adopt"] == "1"
}

// neverFits reports whether target can never be admitted regardless of eviction:
// its ceiling alone exceeds the budget, or its ceiling is unknown (unsizable).
// When the gate is off (pool == 0) nothing is refused this way.
func (s *FIFO) neverFits(target string) bool {
	if s.pool == 0 {
		return false
	}
	c := s.ceilings[target]
	return c <= 0 || c > s.pool-s.reserve
}

// memoryRejectMessage builds the client-facing 503 message for a memory refusal.
func (s *FIFO) memoryRejectMessage(target string) string {
	c := s.ceilings[target]
	if c <= 0 {
		return fmt.Sprintf("model %q has no memoryCeiling configured; cannot admit", target)
	}
	return fmt.Sprintf("model %q needs %d bytes but only %d are available (pool %d - reserve %d)", target, c, s.pool-s.reserve, s.pool, s.reserve)
}

// startSwap records the swap as active and launches it via Effects. running is
// the set EvictionFor saw, forwarded to OnSwapStart so the planner logs against
// the same picture it decided on.
func (s *FIFO) startSwap(initial HandlerReq, evict, running []string) {
	ctx, cancel := context.WithCancel(context.Background())
	s.active[initial.Model] = &activeSwap{
		modelID: initial.Model,
		evict:   evict,
		waiters: []HandlerReq{initial},
		cancel:  cancel,
	}
	s.planner.OnSwapStart(initial.Model, running)
	s.logger.Infof("%s: %s", s.name, swapStartLine(time.Now(), initial, evict))
	s.effects.StartSwap(ctx, initial.Model, evict)
}

// swapStartLine is the INFO line logged when a request starts a model load, so
// a surprise eviction can be traced to whoever asked (grep "swap-start"). It
// also fires for an eviction-only swap (target already Ready, planner evicts
// others). It carries its own UTC timestamp because the logger's is optional
// (logTimeFormat) and journalctl prints local time. Values are quoted so a
// User-Agent with spaces stays one field and a newline cannot forge a line.
func swapStartLine(now time.Time, req HandlerReq, evict []string) string {
	evicting := "none"
	if len(evict) > 0 {
		evicting = strings.Join(evict, ",")
	}
	client := req.Requester.Client
	if client == "" {
		client = "internal"
	}
	line := fmt.Sprintf("swap-start at=%s model=%q evicting=%q client=%q ip=%q method=%q path=%q ua=%q",
		now.UTC().Format(time.RFC3339), req.Model, evicting, client,
		req.Requester.IP, req.Requester.Method, req.Requester.Path, req.Requester.UserAgent)
	if isAdopt(req) {
		line += " trigger=adopt"
	}
	return line
}

// enqueue inserts req into the queue in priority order: it goes just before the
// first queued item whose priority is strictly lower, so higher-priority models
// are serviced first while equal-priority requests keep their arrival (FIFO)
// order. Priorities come from the FifoConfig; unlisted models default to 0.
func (s *FIFO) enqueue(req HandlerReq) {
	p := s.cfg.Priority[req.Model]
	i := len(s.queued)
	for j, q := range s.queued {
		if s.cfg.Priority[q.Model] < p {
			i = j
			break
		}
	}
	s.queued = append(s.queued, HandlerReq{})
	copy(s.queued[i+1:], s.queued[i:])
	s.queued[i] = req
	broadcastQueuePositions(s.queued)
}

// drainQueue walks the queued requests in order, re-running the OnRequest
// decision tree against the (now smaller) active set. Items that can now start
// or join become satisfied; items still blocked remain queued in original order
// and are retried the next time drainQueue runs. That is only on OnSwapDone,
// on OnServeDone when a model's in-flight count reaches zero, on OnUnload, on
// OnLeakGone, on OnLeakStale, and on OnSelfStop (a process that stopped on
// its own: TTL expiry, upstream crash).
func (s *FIFO) drainQueue() {
	// Before the empty check, so a forced TTL stop reported by OnSelfStop is
	// recorded (and its watch started) even when nothing is queued.
	s.recordForcedStops()
	if len(s.queued) == 0 {
		return
	}
	pending := s.queued
	var remaining []HandlerReq
	for _, req := range pending {
		state, ok := s.effects.ModelState(req.Model)
		if !ok {
			s.grantError(req, ErrModelNotFound)
			continue
		}
		if sw, ok := s.active[req.Model]; ok {
			if sw.cancelled {
				remaining = append(remaining, req) // wait for its SwapDone
				continue
			}
			s.logger.Debugf("%s: queued request for model %s now joining in-flight swap", s.name, req.Model)
			sw.waiters = append(sw.waiters, req)
			continue
		}
		// Still stopping: wait for the event that ends the stop (see
		// behindOwnStop in OnRequest).
		if s.pool > 0 && state == process.StateStopping {
			remaining = append(remaining, req)
			continue
		}
		running := s.runningSet(req.Model)
		evict := s.planner.EvictionFor(req.Model, running)
		if state == process.StateReady && len(evict) == 0 && !collidesWith(req.Model, evict, s.active) {
			s.logger.Debugf("%s: queued request for model %s now served fast-path", s.name, req.Model)
			s.grantHandler(req, req.Model)
			continue
		}
		// Memory admission for a queued load: a never-fits request is dropped
		// with an error (grantError releases its reservation). An over-budget
		// one stays queued only while something is still pending that could
		// free memory (memoryPending, the same rule OnRequest applies);
		// otherwise it is refused now, because no later event would drain it.
		// This request was already admitted, so the refusal goes through
		// grantError like any other post-admission failure: a non-streaming
		// caller gets the 503 as-is, and a streaming caller whose loading
		// stream already committed its 200 gets it framed as an SSE error
		// event + [DONE] (ServeHTTP's finishLoading), because that is the only
		// channel left to reach it.
		if !isAdopt(req) && !s.fits(req.Model, evict, running) {
			switch {
			case s.neverFits(req.Model):
				s.logger.Debugf("%s: dropping queued model %s (never fits memory budget)", s.name, req.Model)
				s.grantError(req, swaputil.MemoryAdmissionError{Message: s.memoryRejectMessage(req.Model)})
			case s.memoryPending():
				remaining = append(remaining, req)
			default:
				msg := s.memoryBlockedMessage(req.Model, evict, running)
				s.logger.Warnf("%s: dropping queued model %s: %s", s.name, req.Model, msg)
				s.grantError(req, swaputil.MemoryAdmissionError{Message: msg})
			}
			continue
		}
		if collidesWith(req.Model, evict, s.active) {
			remaining = append(remaining, req)
			continue
		}
		if conflictsWithInFlight(evict, s.inFlight) {
			remaining = append(remaining, req)
			continue
		}
		s.logger.Debugf("%s: queued request for model %s now starting swap, evicting %v", s.name, req.Model, evict)
		s.startSwap(req, evict, running)
	}
	s.queued = remaining
	broadcastQueuePositions(s.queued)
}

// runningSet is the live model set handed to the Swapper: every process the
// baseRouter reports as running, unioned with the targets of in-flight swaps
// (excluding excludeActive, the model whose own swap is being decided — its
// in-flight entry must not count as "already running"). The result is sorted so
// eviction decisions derived from it are deterministic.
func (s *FIFO) runningSet(excludeActive string) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(id string) {
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for id := range s.effects.RunningModels() {
		add(id)
	}
	for _, id := range activeTargets(s.active, excludeActive) {
		add(id)
	}
	sort.Strings(out)
	return out
}

// activeTargets returns the IDs of every in-flight swap target except exclude.
// The planner uses this to account for models committed to but not yet reflected
// in process state.
func activeTargets(active map[string]*activeSwap, exclude string) []string {
	if len(active) == 0 {
		return nil
	}
	out := make([]string, 0, len(active))
	for id := range active {
		if id == exclude {
			continue
		}
		out = append(out, id)
	}
	return out
}

// collidesWith reports whether a new swap with this target and evict set can
// safely run alongside the currently active swaps. Same-target callers should
// JOIN (handled before this) — they do not collide with themselves.
func collidesWith(target string, evict []string, active map[string]*activeSwap) bool {
	for id, sw := range active {
		if id == target {
			continue
		}
		if containsString(evict, id) {
			return true
		}
		if containsString(sw.evict, target) {
			return true
		}
		if slicesOverlap(evict, sw.evict) {
			return true
		}
	}
	return false
}

// slicesOverlap reports whether xs and ys share any common element.
func slicesOverlap(xs, ys []string) bool {
	for _, x := range xs {
		if containsString(ys, x) {
			return true
		}
	}
	return false
}

// conflictsWithInFlight reports whether any model in evict is still handling
// requests. Stopping a busy process would cancel its callers' connections, so
// the scheduler defers the swap until those callers finish.
func conflictsWithInFlight(evict []string, inFlight map[string]int) bool {
	for _, m := range evict {
		if inFlight[m] > 0 {
			return true
		}
	}
	return false
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// broadcastQueuePositions sends each queued request its current 1-indexed
// position. Sends are non-blocking: if the channel is full, the old value is
// drained first so the consumer always sees the latest position.
func broadcastQueuePositions(queued []HandlerReq) {
	for i, req := range queued {
		pos := i + 1
		select {
		case req.PositionCh <- pos:
		default:
			select {
			case <-req.PositionCh:
			default:
			}
			select {
			case req.PositionCh <- pos:
			default:
			}
		}
	}
}
