//go:build !windows

package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// waitForLog polls buf until it contains every string in want or the
// deadline passes. CmdStop runs in its own goroutine and its pipe copy can
// land just after Stop returns.
func waitForLog(buf *syncBuffer, want ...string) bool {
	hasAll := func() bool {
		out := buf.String()
		for _, w := range want {
			if !strings.Contains(out, w) {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(testReturnTimeout)
	for !hasAll() && time.Now().Before(deadline) {
		time.Sleep(testLogPollInterval)
	}
	return hasAll()
}

// TestProcessCommand_CmdStopOutputLogged verifies that stdout and stderr from
// CmdStop are written to the process logger, the same as the start command.
// It lives in this file for the !windows build tag since it needs sh.
func TestProcessCommand_CmdStopOutputLogged(t *testing.T) {
	skipIfNoSimpleResponder(t)

	port := getFreePort(t)
	logBuf := &syncBuffer{}
	procLogger := logmon.NewWriter(logBuf)
	proxyLogger := logmon.NewWriter(io.Discard)

	// printf builds the markers at runtime so they never appear verbatim in
	// the command text, which is itself logged in some paths.
	p, err := New(context.Background(), t.Name(), config.ModelConfig{
		Cmd:                fmt.Sprintf("%s -port %d -silent", simpleResponderPath, port),
		CmdStop:            `sh -c 'printf "cmdstop-%s\n" out; printf "cmdstop-%s\n" err >&2; kill -TERM ${PID}'`,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	}, procLogger, proxyLogger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_ = runAsync(t, p)
	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !waitForLog(logBuf, "cmdstop-out", "cmdstop-err") {
		t.Errorf("expected process log to contain CmdStop stdout and stderr; got:\n%s", logBuf.String())
	}
}

// TestProcessCommand_CmdStopBackgroundChildNotAnError verifies that a CmdStop
// which exits successfully but leaves a background child holding its output
// open is bounded by waitDelay and not reported as a failed stop.
func TestProcessCommand_CmdStopBackgroundChildNotAnError(t *testing.T) {
	skipIfNoSimpleResponder(t)

	port := getFreePort(t)
	logBuf := &syncBuffer{}
	procLogger := logmon.NewWriter(logBuf)
	proxyLogger := logmon.NewWriter(io.Discard)

	p, err := New(context.Background(), t.Name(), config.ModelConfig{
		Cmd:                fmt.Sprintf("%s -port %d -silent", simpleResponderPath, port),
		CmdStop:            `sh -c 'printf "cmdstop-%s\n" bg; sleep 2 & kill -TERM ${PID}'`,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	}, procLogger, proxyLogger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.waitDelay = 200 * time.Millisecond

	_ = runAsync(t, p)
	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !waitForLog(logBuf, "cmdstop-bg", "child held its output open") {
		t.Fatalf("expected CmdStop output and WaitDelay warning in process log; got:\n%s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "stop command failed") {
		t.Errorf("CmdStop that exited cleanly was logged as a failure:\n%s", logBuf.String())
	}
}

// TestProcessCommand_StopForkingWrapper is a regression for the bug reported
// against v219 where Stop would hang indefinitely when the upstream command
// is a shell wrapper that forks the real binary (e.g. `#!/bin/bash` then
// `"$@"`). After SIGTERM the wrapper dies but the grandchild inherits the
// stdout/stderr pipes; cmd.Wait() blocks waiting for the pipe-copy goroutine
// to drain EOF, which never happens while the grandchild holds the fds.
//
// The fix is cmd.WaitDelay (combined with exec.CommandContext + cmd.Cancel),
// which causes the runtime to force-close the pipes after the delay so
// cmd.Wait() — and therefore Stop — returns.
func TestProcessCommand_StopForkingWrapper(t *testing.T) {
	skipIfNoSimpleResponder(t)

	port := getFreePort(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")

	// Wrapper script: backgrounds the child (which inherits stdout/stderr),
	// records its PID for cleanup, then waits. When SIGTERM hits bash it
	// dies without forwarding the signal; the grandchild keeps running and
	// keeps the inherited pipe fds open. This is the scenario reported in
	// the v219 regression.
	wrapper := filepath.Join(dir, "wrapper.sh")
	script := fmt.Sprintf("#!/bin/bash\n%q -port %d -silent &\necho $! > %q\nwait\n",
		simpleResponderPath, port, pidFile)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Cleanup(func() { killChildFromPidFile(pidFile) })

	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                wrapper,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	})
	// Shrink the pipe-close backstop so the test doesn't sit at the
	// production default (10s). Must be set before Run() so doStart picks
	// it up when building the cmd.
	const testWaitDelay = 250 * time.Millisecond
	p.waitDelay = testWaitDelay

	runErr := runAsync(t, p)

	// Stop must return within a bounded time even though the grandchild
	// is still holding the pipe open. Budget is generous on top of
	// testWaitDelay to absorb scheduling jitter on slow CI runners; the
	// pre-fix behaviour was an unbounded hang, so any reasonable cap
	// distinguishes pass from fail.
	stopReturned := make(chan error, 1)
	stopStart := time.Now()
	go func() { stopReturned <- p.Stop(testStopTimeout) }()

	const stopBudget = testWaitDelay + 2*time.Second
	select {
	case err := <-stopReturned:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
		t.Logf("Stop returned in %v", time.Since(stopStart))
	case <-time.After(stopBudget):
		t.Fatalf("Stop did not return within %v — cmd.Wait() likely hung on inherited pipe", stopBudget)
	}

	if got := p.State(); got != StateStopped {
		t.Errorf("after Stop: expected state %s, got %s", StateStopped, got)
	}

	select {
	case <-runErr:
	case <-time.After(testReturnTimeout):
		t.Errorf("Run did not return after Stop")
	}
}

// TestProcessCommand_StopReturnsErrorOnForcedKill pins the contract todo 1.6
// (H2) depends on: an upstream that outlives its graceful window is SIGKILLed,
// and Stop must SAY so instead of returning nil. A forced kill takes down
// llama-swap's supervisor without ever observing the upstream exit — for a model
// whose cmd merely attaches to a container, the container (and its memory)
// survives. The router's memory ledger credits an eviction as freed before it
// loads the next model, so a silent nil there is what lets two ceilings land on
// one pool.
func TestProcessCommand_StopReturnsErrorOnForcedKill(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ignore.ready")

	// Ignore SIGTERM outright, so only the SIGKILL in killProcess's step 3 can
	// end this process. The ready file is written after the trap is installed so
	// the test cannot race the signal ahead of it.
	script := filepath.Join(dir, "ignore-term.sh")
	body := fmt.Sprintf(
		"#!/bin/bash\ntrap '' SIGTERM\necho ready > %q\nwhile true; do sleep 0.1; done\n",
		ready,
	)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	p := newProcessCommand(t, config.ModelConfig{
		Cmd:           script,
		Proxy:         "http://127.0.0.1:1", // unused: health check disabled
		CheckEndpoint: "none",
	})
	p.waitDelay = 200 * time.Millisecond

	runErr := runAsync(t, p)

	trapDeadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(trapDeadline) {
			t.Fatalf("script did not install SIGTERM trap in time")
		}
		time.Sleep(10 * time.Millisecond)
	}

	err := p.Stop(300 * time.Millisecond)
	if !errors.Is(err, ErrForcedKill) {
		t.Fatalf("Stop err=%v want ErrForcedKill", err)
	}
	// The process is still torn down — the error reports HOW, it does not mean
	// the stop was skipped.
	if got := p.State(); got != StateStopped {
		t.Errorf("after Stop: expected state %s, got %s", StateStopped, got)
	}
	select {
	case <-runErr:
	case <-time.After(testReturnTimeout):
		t.Errorf("Run did not return after Stop")
	}
}

// TestProcessCommand_StopHonorsGracefulTimeout is a regression for the bug
// where cmd.WaitDelay capped the graceful shutdown window. killProcess used to
// cancel the cmd context to deliver SIGTERM, which starts cmd.WaitDelay
// immediately; a process whose SIGTERM handler needs longer than WaitDelay to
// finish was force-killed early even though Stop was given a much longer
// timeout. The fix sends the signal directly so WaitDelay measures from process
// exit (its inherited-pipe backstop role), leaving the graceful window to the
// caller's Stop timeout.
func TestProcessCommand_StopHonorsGracefulTimeout(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "graceful.done")
	ready := filepath.Join(dir, "trap.ready")

	// On SIGTERM, sleep past the (short) WaitDelay, then write the marker and
	// exit cleanly. If WaitDelay still drove the kill, bash would be SIGKILLed
	// mid-handler and the marker would never be written. The ready file is
	// written only after the trap is installed so the test does not race
	// SIGTERM ahead of it (CheckEndpoint:none marks ready before bash runs).
	script := filepath.Join(dir, "graceful.sh")
	body := fmt.Sprintf(
		"#!/bin/bash\ncleanup() { sleep 0.6; echo done > %q; exit 0; }\ntrap cleanup SIGTERM\necho ready > %q\nwhile true; do sleep 0.1; done\n",
		marker, ready,
	)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	p := newProcessCommand(t, config.ModelConfig{
		Cmd:           script,
		Proxy:         "http://127.0.0.1:1", // unused: health check disabled
		CheckEndpoint: "none",
	})
	// WaitDelay shorter than the handler's 0.6s sleep, and far shorter than the
	// Stop timeout below — this is the window the old code mis-killed in.
	p.waitDelay = 200 * time.Millisecond

	runErr := runAsync(t, p)

	// Wait until the trap is installed before stopping.
	trapDeadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(trapDeadline) {
			t.Fatalf("script did not install SIGTERM trap in time")
		}
		time.Sleep(10 * time.Millisecond)
	}

	stopStart := time.Now()
	if err := p.Stop(5 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	elapsed := time.Since(stopStart)

	// The handler must have run to completion (marker written) rather than
	// being force-killed at waitDelay.
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("graceful handler did not complete (marker missing): %v", err)
	}
	// And Stop must have waited for the handler (>~0.6s), not returned at the
	// 200ms waitDelay.
	if elapsed < 500*time.Millisecond {
		t.Fatalf("Stop returned in %v — process was killed before its graceful handler finished", elapsed)
	}

	if got := p.State(); got != StateStopped {
		t.Errorf("after Stop: expected state %s, got %s", StateStopped, got)
	}
	select {
	case <-runErr:
	case <-time.After(testReturnTimeout):
		t.Errorf("Run did not return after Stop")
	}
}

// TestProcessCommand_StopReapsForkedGrandchild verifies that stopping a forking
// wrapper takes down the backgrounded grandchild too, rather than leaving it as
// an orphan. The fix is Setpgid (runtime_unix.go): the wrapper leads its own
// process group, so the stop signal is delivered to the whole group via the
// negative PID and reaches the grandchild the wrapper never reaped.
func TestProcessCommand_StopReapsForkedGrandchild(t *testing.T) {
	skipIfNoSimpleResponder(t)

	port := getFreePort(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")

	wrapper := filepath.Join(dir, "wrapper.sh")
	script := fmt.Sprintf("#!/bin/bash\n%q -port %d -silent &\necho $! > %q\nwait\n",
		simpleResponderPath, port, pidFile)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Cleanup(func() { killChildFromPidFile(pidFile) })

	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                wrapper,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	})

	runErr := runAsync(t, p)

	// Read the grandchild PID the wrapper recorded.
	var childPID int
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && pid > 0 {
				childPID = pid
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("wrapper did not record grandchild PID")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// After Stop the grandchild must be gone. Signal 0 probes liveness without
	// actually sending a signal; give it a brief window to exit after the
	// group SIGTERM.
	proc, err := os.FindProcess(childPID)
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	gone := false
	for i := 0; i < 100; i++ {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			gone = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !gone {
		t.Errorf("grandchild PID %d still alive after Stop — process group was not reaped", childPID)
	}

	select {
	case <-runErr:
	case <-time.After(testReturnTimeout):
		t.Errorf("Run did not return after Stop")
	}
}

// killChildFromPidFile reads a PID written by the wrapper script and SIGKILLs
// it so leaked orphans don't accumulate between test runs. Best-effort.
func killChildFromPidFile(pidFile string) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Kill()
}

// TestProcessCommand_DetachDuringStartupSkipsCmdStop is the regression for the
// review finding that a detach-configured model caught mid-start (health still
// failing) fell through doStart's abort() path, which hardcoded detach=false and
// ran CmdStop — tearing down the neighbor container it was meant to preserve.
// The cmd starts and stays alive but never serves health (proxy → a dead port),
// so the process is stuck StateStarting when Detach lands; CmdStop touches a
// marker so we can prove it did NOT run.
func TestProcessCommand_DetachDuringStartupSkipsCmdStop(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "cmdstop.ran")
	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	deadPort := getFreePort(t) // nothing listens here → health check never passes
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                script,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", deadPort),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 30, // long enough to stay StateStarting while we Detach
		CmdStop:            fmt.Sprintf("touch %s", marker),
	})
	p.waitDelay = 250 * time.Millisecond

	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(30 * time.Second) }()

	// Wait until the process is health-checking (StateStarting), not ready.
	deadline := time.Now().Add(3 * time.Second)
	for p.State() != StateStarting {
		if time.Now().After(deadline) {
			t.Fatalf("process never reached StateStarting, got %s", p.State())
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := p.Detach(testStopTimeout); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("CmdStop ran during a detach-in-startup — the neighbor container would have been torn down")
	}
	if got := p.State(); got != StateStopped {
		t.Errorf("after Detach: state = %s, want stopped", got)
	}
	select {
	case <-runErr:
	case <-time.After(testReturnTimeout):
		t.Error("Run did not return after Detach")
	}
}

// TestProcessCommand_StopDuringStartupRunsCmdStop is the companion: a plain Stop
// (not Detach) mid-start MUST still run CmdStop to free the half-started model.
func TestProcessCommand_StopDuringStartupRunsCmdStop(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "cmdstop.ran")
	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	deadPort := getFreePort(t)
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                script,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", deadPort),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 30,
		CmdStop:            fmt.Sprintf("touch %s", marker),
	})
	p.waitDelay = 250 * time.Millisecond

	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(30 * time.Second) }()

	deadline := time.Now().Add(3 * time.Second)
	for p.State() != StateStarting {
		if time.Now().After(deadline) {
			t.Fatalf("process never reached StateStarting, got %s", p.State())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// This CmdStop (touch) does not end the sleep, so the teardown has to
	// force-kill; Stop now reports that instead of the old silent nil.
	if err := p.Stop(testStopTimeout); err != nil && !errors.Is(err, ErrForcedKill) {
		t.Fatalf("Stop: %v", err)
	}
	// CmdStop must have run (marker present) within a brief settle window.
	ran := false
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(marker); err == nil {
			ran = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ran {
		t.Fatal("CmdStop did not run during a plain Stop-in-startup")
	}
	select {
	case <-runErr:
	case <-time.After(testReturnTimeout):
		t.Error("Run did not return after Stop")
	}
}

// writeTermIgnoringScript writes a script that ignores SIGTERM (only SIGKILL
// ends it) and returns its path plus a file it creates once the trap is
// installed. It never listens, so a health check never passes and the process
// stays in StateStarting.
func writeTermIgnoringScript(t *testing.T) (script, ready string) {
	t.Helper()
	dir := t.TempDir()
	script = filepath.Join(dir, "ignore-term.sh")
	ready = filepath.Join(dir, "trap.ready")
	body := fmt.Sprintf("#!/bin/bash\ntrap '' SIGTERM\necho ready > %q\nwhile true; do sleep 0.1; done\n", ready)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return script, ready
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestProcessCommand_StopDuringStartupSurfacesForcedKill covers doStart's
// abort() path: a Stop that lands while the process is still health-checking
// is torn down by abort(), which used to SIGKILL after a hardcoded 5s and throw
// the result away, so a still-tearing-down container read as freed. It must use
// the stop's own timeout and hand ErrForcedKill back to the Stop caller.
func TestProcessCommand_StopDuringStartupSurfacesForcedKill(t *testing.T) {
	script, ready := writeTermIgnoringScript(t)
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:           script,
		Proxy:         fmt.Sprintf("http://127.0.0.1:%d", getFreePort(t)),
		CheckEndpoint: "/health",
		UnloadTimeout: 30, // must NOT be used: the stop's own timeout wins
	})
	p.waitDelay = 200 * time.Millisecond

	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(30 * time.Second) }()
	waitForState(t, p, StateStarting)
	waitForFile(t, ready)

	start := time.Now()
	err := p.Stop(300 * time.Millisecond)
	if !errors.Is(err, ErrForcedKill) {
		t.Fatalf("Stop err=%v want ErrForcedKill", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Stop took %v; abort() should honor the 300ms stop timeout, not a fixed 5s or unloadTimeout", elapsed)
	}
	if got := p.State(); got != StateStopped {
		t.Errorf("after Stop: state=%s want %s", got, StateStopped)
	}
	select {
	case <-runErr:
	case <-time.After(testReturnTimeout):
		t.Error("Run did not return after Stop")
	}
}

// TestProcessCommand_StartFailureSurfacesForcedKill: an intrinsic start failure
// (health check timeout) whose teardown had to force-kill reports that in the
// start error, so the router can keep counting the target's memory.
func TestProcessCommand_StartFailureSurfacesForcedKill(t *testing.T) {
	script, _ := writeTermIgnoringScript(t)
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:           script,
		Proxy:         fmt.Sprintf("http://127.0.0.1:%d", getFreePort(t)),
		CheckEndpoint: "/health",
		UnloadTimeout: 1, // abort()'s graceful window for an intrinsic failure
	})
	p.waitDelay = 200 * time.Millisecond

	err := p.EnsureReady(context.Background(), 1500*time.Millisecond)
	if !errors.Is(err, ErrForcedKill) {
		t.Fatalf("EnsureReady err=%v want it to wrap ErrForcedKill", err)
	}
	if !strings.Contains(err.Error(), "health check timed out") {
		t.Errorf("EnsureReady err=%v lost the start failure", err)
	}
	if got := p.State(); got != StateStopped {
		t.Errorf("state=%s want %s", got, StateStopped)
	}
}
