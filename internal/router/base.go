package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type shutdownReq struct {
	timeout time.Duration
	respond chan error
}

type unloadReq struct {
	targets []string
	timeout time.Duration
	respond chan struct{}
}

// leakGone is sent by a leak watcher when a force-killed model's upstream is
// gone. gen identifies the watch that saw it, so a report from a watch that was
// replaced or cancelled in the meantime is dropped. reason is for the log.
type leakGone struct {
	modelID string
	gen     uint64
	reason  string
}

// selfStop is a process reporting a stop the router did not ask for (a TTL
// unload or an upstream crash, see process.Process.OnSelfStop). err is that
// stop's result; ErrForcedKill means the upstream may still hold its memory.
type selfStop struct {
	modelID string
	err     error
}

// leakWatch is one running leak watcher. Only the run loop touches these.
type leakWatch struct {
	gen    uint64
	cancel context.CancelFunc
}

// Leak-watch probe cadence. The interval is 5s because a force-killed
// container's teardown (compose down after a missed unloadTimeout) takes tens of
// seconds, so a finer poll buys nothing, while a request queued behind the leak
// waits at most one interval past the container actually going away. The
// per-probe timeout is 2s because the probe is a loopback GET that a live
// server answers in milliseconds; a probe that times out is treated as "still
// there" (see upstreamGone), so a short timeout only costs one more attempt.
//
// A runningCheck run gets 10s because it is typically a `docker inspect`, which
// can take seconds against a daemon busy tearing the very container down; a
// run that times out also counts as "still running" (see runningCheckGone), so
// a generous bound only delays the clear, while a tight one could never clear
// on a slow daemon.
const (
	defaultLeakProbeInterval   = 5 * time.Second
	defaultLeakProbeTimeout    = 2 * time.Second
	defaultRunningCheckTimeout = 10 * time.Second
)

// baseRouter owns the channels, run-loop, and process machinery shared by every
// concrete router. Concrete routers embed *baseRouter and supply a
// scheduler.Swapper describing how eviction sets are decided. baseRouter
// implements scheduler.Effects so the scheduler can call back for side-effects.
type baseRouter struct {
	name      string
	config    config.Config
	processes map[string]process.Process
	logger    *logmon.Monitor
	schedule  scheduler.Scheduler

	// shutdownCtx governs the request machinery: cancelling it tells grant()
	// and ServeHTTP to stop granting and reject callers. It is deliberately
	// separate from procCtx — see procCtx below.
	shutdownCtx  context.Context
	shutdownFn   context.CancelFunc
	shuttingDown atomic.Bool

	// procCtx is the parent context for every managed process and governs
	// process lifetime only. handleShutdown stops processes gracefully via
	// Stop() and cancels procCtx afterwards, so teardown is never a context
	// cancel racing the graceful path (which collapsed the grace to 100ms and
	// let the caller return before children were reaped — see process run loop).
	procCtx    context.Context
	procCancel context.CancelFunc

	handlerCh   chan scheduler.HandlerReq
	cancelCh    chan scheduler.HandlerReq
	shutdownCh  chan shutdownReq
	unloadCh    chan unloadReq
	swapDoneCh  chan scheduler.SwapDone
	serveDoneCh chan scheduler.ServeDoneEvent
	leakGoneCh  chan leakGone
	selfStopCh  chan selfStop

	// leakWatches/leakGen are owned by the run loop: WatchLeak and UnwatchLeak
	// are only called from the scheduler, which runs on it. The probe interval
	// and timeout are read there too; tests shorten them before the first
	// request.
	leakWatches         map[string]leakWatch
	leakGen             uint64
	leakProbeInterval   time.Duration
	leakProbeTimeout    time.Duration
	runningCheckTimeout time.Duration

	runDone chan struct{}

	// testProcessed, when non-nil, receives one event after each handlerReq
	// or swapDone has been fully processed by run(). Tests use it to wait
	// for run() to reach a deterministic state without sleeping. serveDone
	// events are intentionally NOT signalled here so test event counts
	// remain stable.
	testProcessed chan struct{}
}

func newBaseRouter(
	name string,
	conf config.Config,
	processes map[string]process.Process,
	logger *logmon.Monitor,
	planner scheduler.Swapper,
) (*baseRouter, error) {
	shutdownCtx, shutdownFn := context.WithCancel(context.Background())
	procCtx, procCancel := context.WithCancel(context.Background())
	b := &baseRouter{
		name:        name,
		config:      conf,
		processes:   processes,
		logger:      logger,
		shutdownCtx: shutdownCtx,
		shutdownFn:  shutdownFn,
		procCtx:     procCtx,
		procCancel:  procCancel,
		handlerCh:   make(chan scheduler.HandlerReq),
		cancelCh:    make(chan scheduler.HandlerReq),
		shutdownCh:  make(chan shutdownReq),
		unloadCh:    make(chan unloadReq),
		swapDoneCh:  make(chan scheduler.SwapDone),
		serveDoneCh: make(chan scheduler.ServeDoneEvent),
		leakGoneCh:  make(chan leakGone),
		selfStopCh:  make(chan selfStop),
		runDone:     make(chan struct{}),

		leakWatches:         make(map[string]leakWatch),
		leakProbeInterval:   defaultLeakProbeInterval,
		leakProbeTimeout:    defaultLeakProbeTimeout,
		runningCheckTimeout: defaultRunningCheckTimeout,
	}
	sched, err := scheduler.New(conf, name, logger, planner, b)
	if err != nil {
		return nil, err
	}
	b.schedule = sched
	// Registered before any process can start, so no self-stop is missed. The
	// report is forwarded from its own goroutine because the callback runs on
	// the process's run loop (upstream crash), which must not wait on the
	// router's: that loop may itself be blocked stopping this very process.
	for id, p := range processes {
		p.OnSelfStop(func(err error) {
			go func() {
				select {
				case b.selfStopCh <- selfStop{modelID: id, err: err}:
				case <-b.shutdownCtx.Done():
				}
			}()
		})
	}
	return b, nil
}

func (b *baseRouter) notifyProcessed() {
	if b.testProcessed != nil {
		b.testProcessed <- struct{}{}
	}
}

func (b *baseRouter) run() {
	defer close(b.runDone)

	for {
		select {
		case req := <-b.shutdownCh:
			b.handleShutdown(req)
			return

		case req := <-b.handlerCh:
			b.schedule.OnRequest(req)
			b.notifyProcessed()

		case req := <-b.cancelCh:
			b.schedule.OnCancel(req)
			b.notifyProcessed()

		case req := <-b.unloadCh:
			b.schedule.OnUnload(req.targets, req.timeout)
			close(req.respond)
			b.notifyProcessed()

		case ev := <-b.swapDoneCh:
			b.schedule.OnSwapDone(ev)
			b.notifyProcessed()

		case ev := <-b.serveDoneCh:
			b.schedule.OnServeDone(ev)

		case ev := <-b.selfStopCh:
			b.schedule.OnSelfStop(ev.modelID, errors.Is(ev.err, process.ErrForcedKill))
			b.notifyProcessed()

		case ev := <-b.leakGoneCh:
			// Only the current watch for the model counts; a stale one lost a
			// race with UnwatchLeak/WatchLeak and its report means nothing now.
			if w, ok := b.leakWatches[ev.modelID]; ok && w.gen == ev.gen {
				w.cancel()
				delete(b.leakWatches, ev.modelID)
				b.schedule.OnLeakGone(ev.modelID, ev.reason)
			}
		}
	}
}

// grant sends a response back to the caller of ServeHTTP and tells us
// whether the caller was still there to receive it.
//
// Each ServeHTTP creates a fresh, UNBUFFERED respond channel and parks in
// a select waiting on it. "Unbuffered" is the important word: a send only
// completes when the other side is actively receiving. So if this send
// succeeds, we know for a fact the caller picked up the response and will
// act on it. If the caller has already given up (its request context was
// cancelled, e.g. the HTTP client disconnected) or the router is shutting
// down, the send never lands, one of the other select cases fires, and we
// report back that the grant did NOT happen.
//
// That distinction matters for in-flight bookkeeping — see GrantServe.
func (b *baseRouter) grant(req scheduler.HandlerReq, resp scheduler.HandlerResp) bool {
	select {
	case req.Respond <- resp:
		return true
	case <-req.Ctx.Done():
		return false
	case <-b.shutdownCtx.Done():
		return false
	}
}

// ModelState implements scheduler.Effects.
func (b *baseRouter) ModelState(modelID string) (process.ProcessState, bool) {
	p, ok := b.processes[modelID]
	if !ok {
		var zero process.ProcessState
		return zero, false
	}
	return p.State(), true
}

// StartSwap implements scheduler.Effects, launching the swap goroutine.
func (b *baseRouter) StartSwap(ctx context.Context, modelID string, evict []string) {
	go b.doSwap(ctx, modelID, evict)
}

// GrantError implements scheduler.Effects.
func (b *baseRouter) GrantError(req scheduler.HandlerReq, err error) {
	b.grant(req, scheduler.HandlerResp{Err: err})
}

// GrantServe implements scheduler.Effects. It hands the caller a wrapped
// p.ServeHTTP (via trackedServe) so the run loop hears about the request
// finishing, and reports whether the caller received it. The scheduler bumps
// its in-flight count only on a true return: if grant() returns false the
// caller already walked away and trackedServe will never run, so no matching
// decrement will ever arrive — incrementing would strand the counter at >0 and
// the router would never again be willing to evict this model.
func (b *baseRouter) GrantServe(req scheduler.HandlerReq, modelID string) bool {
	p := b.processes[modelID]
	return b.grant(req, scheduler.HandlerResp{HandleFunc: b.trackedServe(modelID, p)})
}

// StopProcesses implements scheduler.Effects, stopping the named processes in
// parallel and blocking until all have stopped. It returns the IDs whose stop
// was forced (process.ErrForcedKill).
func (b *baseRouter) StopProcesses(timeout time.Duration, ids []string) []string {
	var wg sync.WaitGroup
	forcedAt := make([]bool, len(ids))
	for i, id := range ids {
		p, ok := b.processes[id]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(idx int, id string, p process.Process) {
			defer wg.Done()
			if err := p.Stop(timeout); err != nil {
				b.logger.Warnf("%s: stopping %s failed: %v", b.name, id, err)
				forcedAt[idx] = errors.Is(err, process.ErrForcedKill)
			}
		}(i, id, p)
	}
	wg.Wait()
	var forced []string
	for i, f := range forcedAt {
		if f {
			forced = append(forced, ids[i])
		}
	}
	return forced
}

// leakProbe runs one check of a leaked model's upstream and reports whether it
// is gone, with the reason for the log when it is.
type leakProbe func(ctx context.Context) (gone bool, reason string)

// WatchLeak implements scheduler.Effects. Every leakProbeInterval it runs the
// model's runningCheck when one is configured (runningCheckGone), else it
// probes the model's proxy URL + checkEndpoint (upstreamGone), and reports
// leakGone once the upstream is gone.
//
// The HTTP probe is only trusted when this run of the upstream was seen
// answering its checkEndpoint (process.UpstreamSeenHealthy): a model killed
// while still loading weights never opened its port, so "connection refused"
// then says nothing about whether its container still holds memory. Such a
// leak, like one with no proxy to probe, gets no watch (returns false): it
// lasts until the model is started again or the owner unloads it, because
// clearing it blind could admit a load on top of memory that is still held.
func (b *baseRouter) WatchLeak(modelID string) bool {
	b.UnwatchLeak(modelID)
	mc := b.config.Models[modelID]
	var probe leakProbe
	if strings.TrimSpace(mc.RunningCheck) != "" {
		args, err := config.SanitizeCommand(mc.RunningCheck)
		if err != nil { // config load validates it; kept as a guard
			b.logger.Warnf("%s: cannot watch leaked model %s: invalid runningCheck: %v", b.name, modelID, err)
			return false
		}
		probe = b.runningCheckProbe(modelID, args, mc.Env)
	} else {
		url := leakProbeURL(mc)
		if url == "" {
			b.logger.Warnf("%s: cannot watch leaked model %s: no proxy URL to probe and no runningCheck", b.name, modelID)
			return false
		}
		if p, ok := b.processes[modelID]; !ok || !p.UpstreamSeenHealthy() {
			b.logger.Warnf("%s: cannot watch leaked model %s: it was never seen answering %s in this run, so a refused connection would not prove it gone; unload it to release its memory, or configure runningCheck", b.name, modelID, url)
			return false
		}
		client := &http.Client{
			Timeout: b.leakProbeTimeout,
			// A fresh connection per probe (no keep-alive, no env proxy): the
			// question is whether anything is listening on the model's port
			// right now, and a pooled connection or an HTTP proxy would answer
			// for it.
			Transport: &http.Transport{DisableKeepAlives: true},
		}
		probe = func(ctx context.Context) (bool, string) {
			return upstreamGone(ctx, client, url), "upstream no longer answers on " + url
		}
	}
	ctx, cancel := context.WithCancel(b.shutdownCtx)
	b.leakGen++
	gen := b.leakGen
	b.leakWatches[modelID] = leakWatch{gen: gen, cancel: cancel}
	go b.watchLeak(ctx, modelID, gen, probe, b.leakProbeInterval)
	return true
}

// RerunStop implements scheduler.Effects. It runs each model's cmdStop again
// (the process is already stopped, so Stop would be a no-op), bounded by
// timeout, in parallel, with the environment the process's commands get. Its
// output goes to the model's process log, like a normal cmdStop's.
func (b *baseRouter) RerunStop(timeout time.Duration, ids []string) []string {
	var wg sync.WaitGroup
	okAt := make([]bool, len(ids))
	for i, id := range ids {
		mc, ok := b.config.Models[id]
		p, hasProc := b.processes[id]
		if !ok || !hasProc {
			continue
		}
		stop := strings.TrimSpace(mc.CmdStop)
		if stop == "" || strings.Contains(stop, "${PID}") {
			b.logger.Warnf("%s: cannot release leaked model %s: its cmdStop is unset or needs ${PID}, and its process is gone; stop its upstream by hand", b.name, id)
			continue
		}
		args, err := config.SanitizeCommand(stop)
		if err != nil {
			b.logger.Warnf("%s: cannot release leaked model %s: invalid cmdStop: %v", b.name, id, err)
			continue
		}
		wg.Add(1)
		go func(idx int, id string, args []string, env []string, out io.Writer) {
			defer wg.Done()
			b.logger.Infof("%s: unload of leaked model %s: running its cmdStop again", b.name, id)
			if err := process.RunBoundedCommand(b.shutdownCtx, args, env, out, timeout); err != nil {
				b.logger.Warnf("%s: cmdStop for leaked model %s did not complete: %v; its memoryCeiling stays counted", b.name, id, err)
				return
			}
			okAt[idx] = true
		}(i, id, args, append(os.Environ(), mc.Env...), p.Logger())
	}
	wg.Wait()
	var done []string
	for i, ok := range okAt {
		if ok {
			done = append(done, ids[i])
		}
	}
	return done
}

// UnwatchLeak implements scheduler.Effects.
func (b *baseRouter) UnwatchLeak(modelID string) {
	if w, ok := b.leakWatches[modelID]; ok {
		w.cancel()
		delete(b.leakWatches, modelID)
	}
}

// watchLeak is the probe loop behind WatchLeak. It never touches router state:
// its only output is the leakGone event, handled on the run loop (whose
// clearLeak logs the reason).
func (b *baseRouter) watchLeak(ctx context.Context, modelID string, gen uint64, probe leakProbe, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		gone, reason := probe(ctx)
		if !gone {
			continue
		}
		select {
		case b.leakGoneCh <- leakGone{modelID: modelID, gen: gen, reason: reason}:
		case <-ctx.Done():
		}
		return
	}
}

// runningCheckProbe builds the leakProbe for a model's runningCheck command.
func (b *baseRouter) runningCheckProbe(modelID string, args, env []string) leakProbe {
	var out io.Writer = io.Discard
	if p, ok := b.processes[modelID]; ok {
		out = p.Logger()
	}
	env = append(os.Environ(), env...)
	timeout := b.runningCheckTimeout
	return func(ctx context.Context) (bool, string) {
		return runningCheckGone(ctx, b.logger, b.name, modelID, args, env, out, timeout)
	}
}

// runningCheckGone runs a leaked model's runningCheck once and reports whether
// its upstream is gone. Only an exit with a non-zero status counts as gone
// (the documented contract: 0 = still running). A run that times out (its
// process group is killed) counts as still running, because a hung check,
// typically a docker CLI waiting on a busy daemon, cannot prove the container
// is gone, and a wrong "gone" admits a load on top of memory that is still
// held while a wrong "running" only costs another interval. A check that
// cannot be started at all (binary missing) is also treated as still running,
// with a warning, for the same reason.
func runningCheckGone(ctx context.Context, logger *logmon.Monitor, name, modelID string, args, env []string, out io.Writer, timeout time.Duration) (bool, string) {
	err := process.RunBoundedCommand(ctx, args, env, out, timeout)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return false, ""
	case errors.As(err, &exitErr):
		return true, fmt.Sprintf("runningCheck exited %d", exitErr.ExitCode())
	case ctx.Err() != nil:
		return false, ""
	case errors.Is(err, process.ErrCommandTimedOut):
		logger.Debugf("%s: runningCheck for leaked model %s timed out after %v; treating it as still running", name, modelID, timeout)
		return false, ""
	default:
		logger.Warnf("%s: runningCheck for leaked model %s could not run: %v; treating it as still running", name, modelID, err)
		return false, ""
	}
}

// upstreamGone reports whether one probe of url shows nothing listening. Only a
// connection-level failure (refused, reset, EOF) counts as gone. Any HTTP
// response, including a 5xx, means a server is still up and so may still hold
// its memory; a timeout cannot tell a gone upstream from a hung one, so it is
// also treated as still there and the next probe decides.
func upstreamGone(ctx context.Context, client *http.Client, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return false
	}
	return true
}

// leakProbeURL is the model's proxy URL + checkEndpoint, or the proxy root when
// the health check is disabled ("none"); "" when there is no proxy.
func leakProbeURL(mc config.ModelConfig) string {
	proxy := strings.TrimRight(strings.TrimSpace(mc.Proxy), "/")
	if proxy == "" {
		return ""
	}
	endpoint := strings.TrimSpace(mc.CheckEndpoint)
	if endpoint == "" || endpoint == "none" {
		endpoint = "/"
	}
	if !strings.HasPrefix(endpoint, "/") {
		endpoint = "/" + endpoint
	}
	return proxy + endpoint
}

// trackedServe is the wrapper that closes the loop on in-flight tracking.
// It runs p.ServeHTTP normally; the only added behaviour is a deferred
// send on serveDoneCh after the handler returns. That send is what tells
// the run loop "this model now has one fewer request in flight — go look
// at the queue again, you may be able to start a swap you previously had
// to defer."
//
// The select on shutdownCtx.Done() is a release valve: if the router is
// already shutting down, nobody is reading serveDoneCh, so we drop the
// notification rather than blocking the HTTP goroutine forever.
func (b *baseRouter) trackedServe(modelID string, p process.Process) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			select {
			case b.serveDoneCh <- scheduler.ServeDoneEvent{ModelID: modelID}:
			case <-b.shutdownCtx.Done():
			}
		}()
		p.ServeHTTP(w, r)
	}
}

// doSwap stops toStop, then starts modelID, and reports the outcome as a
// SwapDone. ctx is cancelled when an unload cancels the swap (FIFO.OnUnload).
func (b *baseRouter) doSwap(ctx context.Context, modelID string, toStop []string) {
	// Evicted models use their configured unloadTimeout; the incoming target
	// uses the (longer) cold-start healthCheckTimeout for its load. Previously
	// both used healthCheckTimeout, so a stuck `docker stop` blocked the whole
	// swap for the full health window and the documented unloadTimeout was
	// silently ignored on the swap path (docs/kb/guides/model-runtime/
	// ttl-and-unloading.md: unloadTimeout "applies to every unload — TTL
	// expiry, a manual unload, or a swap").
	var wg sync.WaitGroup
	stopErrs := make([]error, len(toStop))
	var leaked []string
	for i, mID := range toStop {
		wg.Add(1)
		go func(idx int, p process.Process, id string) {
			defer wg.Done()
			if err := p.Stop(b.unloadTimeout(id)); err != nil {
				b.logger.Warnf("%s: stopping %s failed: %v", b.name, id, err)
				stopErrs[idx] = fmt.Errorf("%s: %w", id, err)
			}
		}(i, b.processes[mID], mID)
	}
	wg.Wait()
	// A forced kill leaves the process Stopped while its upstream may still hold
	// memory; the scheduler keeps charging it (SwapDone.Leaked) until the leak
	// watcher sees it gone, so the NEXT request is not admitted on top of it.
	for i, err := range stopErrs {
		if errors.Is(err, process.ErrForcedKill) {
			leaked = append(leaked, toStop[i])
		}
	}

	// With the memory-admission ledger on, the fit check that authorised this
	// swap CREDITED the evictees' ceilings as freed (residentAfter =
	// (running ∖ evict) ∪ {target}). A stop that did not actually complete makes
	// that credit fiction: starting the target now would put both footprints on
	// the pool at once — the double-count the ledger exists to prevent, on a box
	// whose kernel OOM-killer cannot see model memory. So fail the swap instead.
	// With the gate off (memoryPool == 0) behaviour is unchanged: log and load.
	if b.memoryGateEnabled() {
		if stopErr := errors.Join(stopErrs...); stopErr != nil {
			err := fmt.Errorf("%s: aborting swap to %s, eviction did not complete: %w", b.name, modelID, stopErr)
			b.logger.Errorf("%v", err)
			select {
			case b.swapDoneCh <- scheduler.SwapDone{ModelID: modelID, Err: err, Leaked: leaked}:
			case <-b.shutdownCtx.Done():
			}
			return
		}
	}

	// An unload of the target cancelled this swap while its evictions ran: do
	// not start what the unload just asked to stop. The scheduler still holds
	// the swap's entry, so the target and evictees stay charged until this
	// SwapDone. The check cannot be atomic with EnsureReady (a cancel can land
	// just after it); FIFO.OnSwapDone stops a target that started that way.
	if ctx.Err() != nil {
		err := fmt.Errorf("%s: swap to %s cancelled: model unloaded", b.name, modelID)
		b.logger.Infof("%v", err)
		select {
		case b.swapDoneCh <- scheduler.SwapDone{ModelID: modelID, Err: err, Leaked: leaked}:
		case <-b.shutdownCtx.Done():
		}
		return
	}

	// EnsureReady rather than a State() check followed by Run: the router must
	// not assume anything about the process. Deciding out here means acting on
	// a snapshot that the process's own run loop can invalidate at any moment —
	// a TTL unload landing in that window used to leave the swap waiting on a
	// process nobody was ever going to start (issue #946). EnsureReady makes
	// the same decision inside the process, where the state is owned.
	target := b.processes[modelID]
	err := target.EnsureReady(b.shutdownCtx, b.healthCheckTimeout())
	if err != nil && b.shutdownCtx.Err() == nil {
		// Quiet during shutdown: every in-flight swap fails at once there, and
		// that is expected rather than worth a warning per model.
		b.logger.Warnf("%s: starting %s failed: %v", b.name, modelID, err)
	}
	// A failed start whose own teardown was forced (process wraps it into the
	// start error) may have left the target's container up too.
	if errors.Is(err, process.ErrForcedKill) {
		leaked = append(leaked, modelID)
	}

	select {
	case b.swapDoneCh <- scheduler.SwapDone{ModelID: modelID, Err: err, Leaked: leaked}:
	case <-b.shutdownCtx.Done():
	}
}

func (b *baseRouter) handleShutdown(req shutdownReq) {
	shutdownErr := fmt.Errorf("%s is shutting down", b.name)

	// Cancel shutdownCtx first so any waiter that is currently parked on
	// its respond channel can exit via its own shutdownCtx.Done() branch.
	// The OnShutdown grants below then either land (waiter happened to receive
	// before noticing shutdown) or fall through immediately via grant's
	// shutdownCtx case — either way the waiter sees a non-OK response.
	// This does NOT touch processes: their lifetime is procCtx, cancelled
	// only after the graceful Stop() calls below have reaped them.
	b.shutdownFn()

	b.schedule.OnShutdown(shutdownErr)

	stopTimeout := req.timeout
	if stopTimeout <= 0 {
		stopTimeout = b.healthCheckTimeout()
	}

	var wg sync.WaitGroup
	for i, p := range b.processes {
		wg.Add(1)
		go func(id string, p process.Process) {
			defer wg.Done()
			// Detach (skip CmdStop, leave the upstream container running) for
			// models configured that way, so a neighbor container survives
			// llama-swap's own shutdown/reload; adopt re-attaches on start.
			// Swaps and TTL unloads still go through Stop, which frees memory.
			var err error
			if b.detachOnShutdown(id) {
				err = p.Detach(stopTimeout)
			} else {
				err = p.Stop(stopTimeout)
			}
			if err != nil {
				b.logger.Warnf("%s failed to stop process %s: %v", b.name, id, err)
			}
		}(i, p)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	if req.timeout > 0 {
		select {
		case <-done:
		case <-time.After(req.timeout):
			<-done
		}
	} else {
		<-done
	}

	// Every process is stopped (children reaped via Stop()). Cancel procCtx so
	// the process run-loop goroutines exit; they are already StateStopped, so
	// this is a clean no-op kill rather than a forced teardown.
	b.procCancel()

	req.respond <- nil
}

func (b *baseRouter) healthCheckTimeout() time.Duration {
	t := time.Duration(b.config.HealthCheckTimeout) * time.Second
	if t <= 0 {
		return 30 * time.Second
	}
	return t
}

// unloadTimeout returns the graceful stop timeout for a model. Config parsing
// guarantees both the global and per-model unloadTimeout are populated (a zero
// model value is rewritten to the global default on parse), so no zero handling
// is needed here.
func (b *baseRouter) unloadTimeout(modelID string) time.Duration {
	if mc, ok := b.config.Models[modelID]; ok {
		return time.Duration(mc.UnloadTimeout) * time.Second
	}
	return time.Duration(b.config.UnloadTimeout) * time.Second
}

// memoryGateEnabled reports whether the memory-admission ledger is active. It is
// the same switch the scheduler uses (FIFO.fits early-returns when pool == 0), so
// an unconfigured box keeps exactly today's swap behaviour.
func (b *baseRouter) memoryGateEnabled() bool {
	return b.config.MemoryPool > 0
}

// detachOnShutdown reports whether the model should be detached (upstream left
// running, CmdStop skipped) rather than stopped when llama-swap shuts down.
func (b *baseRouter) detachOnShutdown(modelID string) bool {
	mc, ok := b.config.Models[modelID]
	return ok && mc.DetachOnShutdown
}

func (b *baseRouter) Handles(model string) bool {
	_, ok := b.processes[model]
	return ok
}

func (b *baseRouter) ProcessLogger(modelID string) (*logmon.Monitor, bool) {
	if p, ok := b.processes[modelID]; ok {
		return p.Logger(), true
	}
	return nil, false
}

// RunningModels returns the current state of every process that is not stopped
// or shut down. The processes map keys are fixed at construction and State()
// is a snapshot, so this is safe to call without the run loop.
func (b *baseRouter) RunningModels() map[string]process.ProcessState {
	running := make(map[string]process.ProcessState)
	for id, st := range b.RunningStatus() {
		running[id] = st.State
	}
	return running
}

// RunningStatus returns the status snapshot of every process that is not
// stopped or shut down.
func (b *baseRouter) RunningStatus() map[string]process.Status {
	running := make(map[string]process.Status)
	for id, p := range b.processes {
		st := p.Status()
		if st.State == process.StateStopped || st.State == process.StateShutdown {
			continue
		}
		running[id] = st
	}
	return running
}

// Unload stops the named models, or every running model when none are named.
// It blocks until each targeted process has stopped.
//
// The request is funneled through the run loop so eviction is coordinated
// with the rest of the router's state: pending swap waiters for an
// unloaded model are released with an error, queued requests for unloaded
// models are dropped, and any deferred swaps that were waiting on those
// models become eligible to start.
//
// In-flight requests being served by an unloaded process are not waited
// for — Stop kills the upstream, those callers see whatever error the
// reverse proxy surfaces and may retry. Their trackedServe defers fire
// normally and decrement inFlight as the dying handlers return.
//
// A timeout <= 0 unloads each targeted model with its configured
// unloadTimeout: targets sharing a timeout are stopped in parallel within one
// unload request, and the requests are processed smallest timeout first. The
// requests are sequential, so a hung stop on a large model (long timeouts
// usually mean multi-node unloads) cannot delay reclaiming the quick ones
// queued behind it. A positive timeout overrides the configured values and
// stops every target with that timeout.
func (b *baseRouter) Unload(timeout time.Duration, models ...string) {
	targets := models
	if len(targets) == 0 {
		targets = make([]string, 0, len(b.processes))
		for id := range b.processes {
			targets = append(targets, id)
		}
	}
	if len(targets) == 0 {
		return
	}

	if timeout > 0 {
		b.sendUnload(targets, timeout)
		return
	}
	buckets := make(map[time.Duration][]string)
	for _, id := range targets {
		t := b.unloadTimeout(id)
		buckets[t] = append(buckets[t], id)
	}
	timeouts := make([]time.Duration, 0, len(buckets))
	for t := range buckets {
		timeouts = append(timeouts, t)
	}
	sort.Slice(timeouts, func(i, j int) bool { return timeouts[i] < timeouts[j] })
	for _, t := range timeouts {
		b.sendUnload(buckets[t], t)
	}
}

// sendUnload funnels one unload request through the run loop and blocks until
// the scheduler has stopped the targeted processes.
func (b *baseRouter) sendUnload(targets []string, timeout time.Duration) {
	req := unloadReq{targets: targets, timeout: timeout, respond: make(chan struct{})}
	select {
	case b.unloadCh <- req:
	case <-b.runDone:
		return
	}
	<-req.respond
}

func (b *baseRouter) Shutdown(timeout time.Duration) error {
	if !b.shuttingDown.CompareAndSwap(false, true) {
		return fmt.Errorf("%s shutdown already in progress", b.name)
	}
	req := shutdownReq{timeout: timeout, respond: make(chan error, 1)}
	select {
	case b.shutdownCh <- req:
	case <-b.runDone:
		return nil
	}
	return <-req.respond
}

func (b *baseRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if b.shuttingDown.Load() {
		swaputil.SendError(w, req, fmt.Errorf("%s is shutting down", b.name))
		return
	}

	data, err := swaputil.FetchContext(req, b.config)
	if err != nil {
		swaputil.SendError(w, req, err)
		return
	}

	// Ignored websocket connections are deliberately kept outside the
	// scheduler: they cannot start or queue a model, consume concurrency, or
	// prevent another request from swapping the process out. A process may stop
	// immediately after this readiness check; dropping that websocket is the
	// intended tradeoff of opting out of lifecycle tracking.
	if swaputil.ShouldIgnoreWebsocket(req, b.config) {
		p, ok := b.processes[data.ModelID]
		if !ok {
			swaputil.SendError(w, req, scheduler.ErrModelNotFound)
			return
		}
		if p.State() != process.StateReady {
			swaputil.SendResponse(w, req, http.StatusConflict,
				fmt.Sprintf("model %s is not loaded; ignored websocket requests cannot start it", data.ModelID))
			return
		}
		p.ServeHTTP(w, req)
		return
	}

	hr := scheduler.HandlerReq{
		Model: data.ModelID,
		Ctx:   req.Context(),
		// Unbuffered: a successful send on Respond proves the waiter is
		// alive and consuming. grant() relies on this to avoid handing a
		// handleFunc to a cancelled waiter and leaking the inFlight count.
		Admit:      make(chan error, 1),
		Respond:    make(chan scheduler.HandlerResp),
		PositionCh: make(chan int, 1),
	}

	select {
	case b.handlerCh <- hr:
	case <-req.Context().Done():
		return
	case <-b.shutdownCtx.Done():
		swaputil.SendError(w, req, fmt.Errorf("%s is shutting down", b.name))
		return
	}

	var admissionErr error
	select {
	case admissionErr = <-hr.Admit:
	case <-req.Context().Done():
		select {
		case b.cancelCh <- hr:
		case <-b.shutdownCtx.Done():
		}
		return
	case <-b.shutdownCtx.Done():
		swaputil.SendError(w, req, fmt.Errorf("%s is shutting down", b.name))
		return
	}
	if admissionErr != nil {
		swaputil.SendError(w, req, admissionErr)
		return
	}

	isModelReady := false
	if p, ok := b.processes[data.ModelID]; ok {
		isModelReady = p.State() == process.StateReady
	}
	shouldShowLoading := data.Streaming && data.SendLoadingState && isLoadingPath(req.URL.Path) && !isModelReady

	var lw *loadingWriter
	cancelLoad := func() {}
	if shouldShowLoading {
		var swapCtx context.Context
		swapCtx, cancelLoad = context.WithCancel(req.Context())
		lw = newLoadingWriter(b.logger, data.ModelID, w, req)
		go lw.start(swapCtx)
		go func() {
			for {
				select {
				case pos := <-hr.PositionCh:
					lw.setUpdate(fmt.Sprintf("Queue position: #%d", pos))
				case <-swapCtx.Done():
					return
				}
			}
		}()
	}

	// finishLoading stops the loading stream and fences its goroutine off from
	// the ResponseWriter before the real handler (or ServeHTTP's return)
	// reclaims it. release() must run even when waitForCompletion times out:
	// otherwise a still-streaming goroutine flushes a finalized response and
	// panics on the recycled *bufio.Writer.
	//
	// A non-nil streamErr is framed into the stream first, while writes still
	// reach the client: the 200 is already committed, so this is the only way
	// the error can be reported at all.
	finishLoading := func(streamErr error) {
		cancelLoad()
		if lw != nil {
			lw.waitForCompletion(1 * time.Second)
			if streamErr != nil {
				lw.sendError(streamErr)
			}
			lw.release()
		}
	}

	// reportError sends err as a normal error response, for the paths where no
	// loading stream was live to carry it in-band. When one was, finishLoading
	// has already framed it into the stream and a second report would append a
	// bare JSON line that SSE parsers discard.
	reportError := func(err error) {
		if lw == nil {
			swaputil.SendError(w, req, err)
		}
	}

	var resp scheduler.HandlerResp
	select {
	case resp = <-hr.Respond:
		// Pass the dispatch error in so it is framed into the stream before
		// release fences the writer; a nil error just ends the stream.
		finishLoading(resp.Err)
	case <-req.Context().Done():
		// The client is gone, so there is nobody to report to.
		finishLoading(nil)
		// Notify the scheduler so it can prune this request from its queue
		// and swap waiters. Without this, a queued request whose client left
		// would sit in the scheduler until drainQueue eventually starts a
		// wasted model load for it.
		select {
		case b.cancelCh <- hr:
		case <-b.shutdownCtx.Done():
		}
		return
	case <-b.shutdownCtx.Done():
		shutdownErr := fmt.Errorf("%s is shutting down", b.name)
		finishLoading(shutdownErr)
		reportError(shutdownErr)
		return
	}

	if resp.Err != nil {
		reportError(resp.Err)
		return
	}
	resp.HandleFunc(w, req)
}
