package scheduler

import (
	"fmt"
	"sort"
	"strconv"
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
	}
}

// OnRequest decides what to do with one incoming ServeHTTP request. It never
// blocks indefinitely: any work that has to wait (starting a process, stopping
// siblings, waiting for ready) is deferred to a swap goroutine and reported back
// via OnSwapDone.
//
// Two refusals are answered on the ADMISSION channel rather than after
// admission: an unknown model, and a new load the memory budget can never fit
// (step (1.5) in the body). That seam is before the caller can start a loading
// stream, which is what keeps them clean status codes.
//
// The decision tree, in order:
//
//  1. Unknown model — respond with ErrModelNotFound and move on.
//  2. A swap to the same model is already in flight — attach this waiter so
//     one swap serves all callers that asked for the same model.
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
	sw, joining := s.active[req.Model]
	var (
		running, evict []string
		fastPath       bool
	)
	if !joining {
		running = s.runningSet(req.Model)
		evict = s.planner.EvictionFor(req.Model, running)
		// (3) Fast path: ready, nothing to evict, and nobody is evicting us.
		fastPath = state == process.StateReady && len(evict) == 0 && !collidesWith(req.Model, evict, s.active)
	}

	// (1.5) Hard memory refusal, decided BEFORE admission succeeds. The model's
	// own ceiling exceeds the budget (or is unknown), so no eviction and no
	// waiting can ever help — this is a 503 the scheduler can answer at once.
	// Answering it on the admission channel is what keeps it a clean 503: past
	// admit(), a streaming caller has already been handed 200 + SSE headers by
	// the loading writer, and the refusal can then only be framed into the
	// stream (#1029's failure-reported-as-success shape). This is the same
	// pre-stream seam the concurrency-limit rejection uses. neverFits implies
	// !fits, so the gate after admission only has the transient case left.
	if !joining && !fastPath && !isAdopt(req) && s.neverFits(req.Model) {
		s.logger.Debugf("%s: refusing model %s (never fits memory budget)", s.name, req.Model)
		s.rejectAdmission(req, swaputil.MemoryAdmissionError{Message: s.memoryRejectMessage(req.Model)})
		return
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

	// (3) Fast path.
	if fastPath {
		s.logger.Debugf("%s: fast-path serving model %s (already ready)", s.name, req.Model)
		s.grantHandler(req, req.Model)
		return
	}

	// Memory admission: a NEW load must fit the pool once the evict set frees.
	// Only the transient case reaches here (the never-fits half was answered
	// before admission), so it waits for memory to free rather than refusing.
	if !isAdopt(req) && !s.fits(req.Model, evict, running) {
		s.logger.Debugf("%s: queuing model %s (does not fit now; waiting for memory to free)", s.name, req.Model)
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
	sw, ok := s.active[ev.ModelID]
	if !ok {
		return
	}
	delete(s.active, ev.ModelID)

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

	targetSet := make(map[string]bool, len(targets))
	for _, id := range targets {
		targetSet[id] = true
	}

	// Release waiters of any in-flight swap whose target is being unloaded.
	// The swap goroutine itself is left to finish on its own; when its
	// SwapDone arrives, OnSwapDone will find no entry in active and drop it.
	for id := range targetSet {
		sw, ok := s.active[id]
		if !ok {
			continue
		}
		for _, w := range sw.waiters {
			s.grantError(w, unloadErr)
		}
		delete(s.active, id)
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

	// Stop the targeted processes. Done synchronously so Unload's caller can
	// rely on "after Unload returns, the process is stopped". inFlight is
	// intentionally NOT cleared here: each dying handler will fire its tracked
	// serve and reach OnServeDone in the normal way.
	s.effects.StopProcesses(timeout, targets)

	// Removing entries from active above may have unblocked queued requests
	// that previously collided with the now-cancelled swaps.
	s.drainQueue()
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
// treats that as a hard refuse via neverFits). running is the pre-swap resident +
// in-flight set and excludes target. A RESIDENT model with no ceiling is a config
// error: counted as 0 (best-effort) rather than deadlocking the queue — the
// shipped earlyoom + fail-closed compose gate backstop an actual OOM.
func (s *FIFO) fits(target string, evict, running []string) bool {
	if s.pool == 0 {
		return true
	}
	if s.ceilings[target] <= 0 {
		return false
	}
	budget := s.pool - s.reserve
	evicted := make(map[string]struct{}, len(evict))
	for _, id := range evict {
		evicted[id] = struct{}{}
	}
	needed := s.ceilings[target]
	if needed > budget {
		return false
	}
	for _, id := range running {
		if id == target {
			continue
		}
		if _, gone := evicted[id]; gone {
			continue
		}
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
	s.active[initial.Model] = &activeSwap{
		modelID: initial.Model,
		evict:   evict,
		waiters: []HandlerReq{initial},
	}
	s.planner.OnSwapStart(initial.Model, running)
	s.effects.StartSwap(initial.Model, evict)
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
// so they get another chance on the next swap completion.
func (s *FIFO) drainQueue() {
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
			s.logger.Debugf("%s: queued request for model %s now joining in-flight swap", s.name, req.Model)
			sw.waiters = append(sw.waiters, req)
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
		// with an error (grantError releases its reservation); a transient
		// over-budget stays queued to retry on the next residency release.
		if !isAdopt(req) && !s.fits(req.Model, evict, running) {
			if s.neverFits(req.Model) {
				s.logger.Debugf("%s: dropping queued model %s (never fits memory budget)", s.name, req.Model)
				s.grantError(req, swaputil.MemoryAdmissionError{Message: s.memoryRejectMessage(req.Model)})
			} else {
				remaining = append(remaining, req)
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
