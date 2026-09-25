package supervisor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	supervisorv1 "github.com/andreabedini/minecraft-operator/gen/supervisor/v1"
)

const (
	defaultStopTimeout = 90 * time.Second
	defaultHookTimeout = 60 * time.Second
	minBackoff         = time.Second
	maxBackoff         = time.Minute
	// A run longer than this resets the restart backoff.
	stableRunDuration = 5 * time.Minute
)

var (
	// ErrNotRunning is returned by operations that need a live process.
	ErrNotRunning = errors.New("process is not running")
	// ErrRunning is returned by operations that need the process stopped.
	ErrRunning = errors.New("process is running")
	// ErrNoLaunchSpec is returned by Start when no launch spec has been set.
	ErrNoLaunchSpec = errors.New("no launch spec set")
)

// ExitInfo describes the last process exit.
type ExitInfo struct {
	Code int
	At   time.Time
}

// Snapshot is a point-in-time view of the managed process.
type Snapshot struct {
	State     supervisorv1.ProcessState
	PID       int
	StartedAt time.Time
	LastExit  *ExitInfo
	Spec      *supervisorv1.LaunchSpec
	SpecHash  string
}

// Manager runs the single supervised process described by a launch spec.
type Manager struct {
	root    Root
	state   *State
	console *Console
	logger  *slog.Logger
	// Passthrough receives the process's stdout and stderr lines so the
	// container log carries them. Nil disables it.
	Passthrough io.Writer

	mu            sync.Mutex
	spec          *supervisorv1.LaunchSpec
	specHash      string
	procState     supervisorv1.ProcessState
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	startedAt     time.Time
	lastExit      *ExitInfo
	stopRequested bool
	done          chan struct{}
	backoff       time.Duration
	restartTimer  *time.Timer
}

// NewManager creates a manager. The spec, if any, is loaded from state.
func NewManager(root Root, state *State, console *Console, logger *slog.Logger) (*Manager, error) {
	m := &Manager{
		root:        root,
		state:       state,
		console:     console,
		logger:      logger,
		Passthrough: os.Stdout,
		procState:   supervisorv1.ProcessState_PROCESS_STATE_STOPPED,
		backoff:     minBackoff,
	}
	spec, err := state.LoadLaunch()
	if err != nil {
		return nil, err
	}
	if spec != nil {
		m.spec = spec
		m.specHash = LaunchHash(spec)
	}
	return m, nil
}

// SetSpec validates and persists a new launch spec. The running process, if
// any, keeps its old spec until restarted.
func (m *Manager) SetSpec(spec *supervisorv1.LaunchSpec) (string, error) {
	if err := validateSpec(spec); err != nil {
		return "", err
	}
	if _, err := m.root.Resolve(spec.GetWorkingDir()); err != nil {
		return "", fmt.Errorf("working_dir: %w", err)
	}
	spec = proto.Clone(spec).(*supervisorv1.LaunchSpec)
	if err := m.state.SaveLaunch(spec); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spec = spec
	m.specHash = LaunchHash(spec)
	return m.specHash, nil
}

func validateSpec(spec *supervisorv1.LaunchSpec) error {
	if spec == nil {
		return errors.New("launch spec is required")
	}
	if strings.TrimSpace(spec.GetCommand()) == "" {
		return errors.New("launch spec: command is required")
	}
	for name, t := range spec.GetTunnelTargets() {
		if err := validateTunnelTarget(t.GetAddress()); err != nil {
			return fmt.Errorf("tunnel target %q: %w", name, err)
		}
	}
	for _, h := range []*supervisorv1.ConsoleHook{spec.GetQuiesce(), spec.GetResume()} {
		if h.GetWaitFor() != "" {
			if _, err := regexp.Compile(h.GetWaitFor()); err != nil {
				return fmt.Errorf("hook wait_for: %w", err)
			}
		}
	}
	return nil
}

// Spec returns a copy of the current launch spec, or nil.
func (m *Manager) Spec() *supervisorv1.LaunchSpec {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.spec == nil {
		return nil
	}
	return proto.Clone(m.spec).(*supervisorv1.LaunchSpec)
}

// Status returns a snapshot.
func (m *Manager) Status() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{
		State:     m.procState,
		StartedAt: m.startedAt,
		SpecHash:  m.specHash,
	}
	if m.cmd != nil && m.cmd.Process != nil && m.procState != supervisorv1.ProcessState_PROCESS_STATE_STOPPED {
		s.PID = m.cmd.Process.Pid
	}
	if m.lastExit != nil {
		e := *m.lastExit
		s.LastExit = &e
	}
	if m.spec != nil {
		s.Spec = proto.Clone(m.spec).(*supervisorv1.LaunchSpec)
	}
	return s
}

// Running reports whether the process is alive (running or stopping).
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.procState != supervisorv1.ProcessState_PROCESS_STATE_STOPPED
}

// Start launches the process from the current spec.
func (m *Manager) Start(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cancelPendingRestartLocked()
	return m.startLocked()
}

func (m *Manager) startLocked() (int, error) {
	if m.procState != supervisorv1.ProcessState_PROCESS_STATE_STOPPED {
		return 0, ErrRunning
	}
	if m.spec == nil {
		return 0, ErrNoLaunchSpec
	}
	spec := m.spec
	dir, err := m.root.Resolve(spec.GetWorkingDir())
	if err != nil {
		return 0, fmt.Errorf("working_dir: %w", err)
	}
	cmd := exec.Command(spec.GetCommand(), spec.GetArgs()...)
	cmd.Dir = dir
	cmd.Env = mergedEnv(spec.GetEnv())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	m.cmd = cmd
	m.stdin = stdin
	m.procState = supervisorv1.ProcessState_PROCESS_STATE_RUNNING
	m.startedAt = time.Now()
	m.stopRequested = false
	done := make(chan struct{})
	m.done = done
	pid := cmd.Process.Pid
	m.console.Append(supervisorv1.Stream_STREAM_SUPERVISOR, fmt.Sprintf("started pid %d: %s", pid, strings.Join(append([]string{spec.GetCommand()}, spec.GetArgs()...), " ")))
	m.logger.Info("process started", "pid", pid, "command", spec.GetCommand())

	var wg sync.WaitGroup
	wg.Add(2)
	go m.pump(&wg, stdout, supervisorv1.Stream_STREAM_STDOUT)
	go m.pump(&wg, stderr, supervisorv1.Stream_STREAM_STDERR)
	go m.wait(cmd, &wg, done)
	return pid, nil
}

func mergedEnv(extra map[string]string) []string {
	env := os.Environ()
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// pump copies lines from the process to the console and the passthrough.
func (m *Manager) pump(wg *sync.WaitGroup, r io.Reader, stream supervisorv1.Stream) {
	defer wg.Done()
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			text := strings.TrimRight(line, "\r\n")
			m.console.Append(stream, text)
			if m.Passthrough != nil {
				_, _ = io.WriteString(m.Passthrough, text+"\n")
			}
		}
		if err != nil {
			return
		}
	}
}

func (m *Manager) wait(cmd *exec.Cmd, pumps *sync.WaitGroup, done chan struct{}) {
	pumps.Wait()
	err := cmd.Wait()
	code := exitCode(cmd, err)

	m.mu.Lock()
	m.procState = supervisorv1.ProcessState_PROCESS_STATE_STOPPED
	m.lastExit = &ExitInfo{Code: code, At: time.Now()}
	runFor := time.Since(m.startedAt)
	requested := m.stopRequested
	policy := m.spec.GetRestartPolicy()
	m.cmd = nil
	m.stdin = nil
	close(done)
	m.console.Append(supervisorv1.Stream_STREAM_SUPERVISOR, fmt.Sprintf("process exited with code %d after %s", code, runFor.Round(time.Second)))
	m.logger.Info("process exited", "code", code, "duration", runFor, "requested", requested)

	if requested {
		m.mu.Unlock()
		return
	}
	restart := false
	switch policy {
	case supervisorv1.RestartPolicy_RESTART_POLICY_ALWAYS:
		restart = true
	case supervisorv1.RestartPolicy_RESTART_POLICY_ON_FAILURE:
		restart = code != 0
	}
	if !restart {
		m.mu.Unlock()
		return
	}
	if runFor > stableRunDuration {
		m.backoff = minBackoff
	}
	delay := m.backoff
	m.backoff = min(m.backoff*2, maxBackoff)
	m.console.Append(supervisorv1.Stream_STREAM_SUPERVISOR, fmt.Sprintf("restarting in %s", delay))
	m.restartTimer = time.AfterFunc(delay, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.restartTimer = nil
		if _, err := m.startLocked(); err != nil {
			m.logger.Error("automatic restart failed", "error", err)
			m.console.Append(supervisorv1.Stream_STREAM_SUPERVISOR, "automatic restart failed: "+err.Error())
		}
	})
	m.mu.Unlock()
}

func (m *Manager) cancelPendingRestartLocked() {
	if m.restartTimer != nil {
		m.restartTimer.Stop()
		m.restartTimer = nil
	}
}

func exitCode(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState != nil {
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			return code
		}
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
	}
	if err != nil {
		return -1
	}
	return 0
}

// WriteInput writes one line to the process's stdin.
func (m *Manager) WriteInput(line string) error {
	m.mu.Lock()
	stdin := m.stdin
	running := m.procState == supervisorv1.ProcessState_PROCESS_STATE_RUNNING
	m.mu.Unlock()
	if !running || stdin == nil {
		return ErrNotRunning
	}
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	_, err := io.WriteString(stdin, line)
	return err
}

// Stop asks the process to exit and waits. With kill it is killed at once.
// A zero timeout uses the spec's stop_timeout or the default. It returns the
// exit code, or the last exit code if the process was already stopped.
func (m *Manager) Stop(ctx context.Context, timeout time.Duration, kill bool) (int, error) {
	m.mu.Lock()
	m.cancelPendingRestartLocked()
	if m.procState == supervisorv1.ProcessState_PROCESS_STATE_STOPPED {
		code := 0
		if m.lastExit != nil {
			code = m.lastExit.Code
		}
		m.mu.Unlock()
		return code, nil
	}
	m.stopRequested = true
	cmd := m.cmd
	stdin := m.stdin
	done := m.done
	spec := m.spec
	alreadyStopping := m.procState == supervisorv1.ProcessState_PROCESS_STATE_STOPPING
	m.procState = supervisorv1.ProcessState_PROCESS_STATE_STOPPING
	m.mu.Unlock()

	if timeout <= 0 {
		timeout = spec.GetStopTimeout().AsDuration()
	}
	if timeout <= 0 {
		timeout = defaultStopTimeout
	}

	if kill {
		m.console.Append(supervisorv1.Stream_STREAM_SUPERVISOR, "killing process")
		_ = cmd.Process.Kill()
	} else if !alreadyStopping {
		if sc := spec.GetStopCommand(); sc != "" {
			m.console.Append(supervisorv1.Stream_STREAM_SUPERVISOR, "sending stop command")
			if _, err := io.WriteString(stdin, sc+"\n"); err != nil {
				m.logger.Warn("stop command write failed, sending SIGTERM", "error", err)
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
		} else {
			m.console.Append(supervisorv1.Stream_STREAM_SUPERVISOR, "sending SIGTERM")
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	}

	select {
	case <-done:
	case <-time.After(timeout):
		m.console.Append(supervisorv1.Stream_STREAM_SUPERVISOR, fmt.Sprintf("stop timed out after %s, killing", timeout))
		m.logger.Warn("stop timed out, killing", "timeout", timeout)
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastExit != nil {
		return m.lastExit.Code, nil
	}
	return 0, nil
}

// Restart stops (if running) and starts again.
func (m *Manager) Restart(ctx context.Context, stopTimeout time.Duration) (int, error) {
	if _, err := m.Stop(ctx, stopTimeout, false); err != nil {
		return 0, err
	}
	return m.Start(ctx)
}

// RunHook writes the hook's commands to stdin and, if wait_for is set, waits
// for a matching console line. A nil hook or one without commands and
// without wait_for is a no-op.
func (m *Manager) RunHook(ctx context.Context, hook *supervisorv1.ConsoleHook) error {
	if hook == nil || (len(hook.GetCommands()) == 0 && hook.GetWaitFor() == "") {
		return nil
	}
	if !m.Running() {
		return ErrNotRunning
	}
	var (
		re  *regexp.Regexp
		err error
	)
	if hook.GetWaitFor() != "" {
		re, err = regexp.Compile(hook.GetWaitFor())
		if err != nil {
			return err
		}
	}
	timeout := hook.GetTimeout().AsDuration()
	if timeout <= 0 {
		timeout = defaultHookTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		matched = make(chan error, 1)
	)
	if re != nil {
		ch, unsubscribe := m.console.Subscribe(0)
		go func() {
			defer unsubscribe()
			for {
				select {
				case <-ctx.Done():
					matched <- ctx.Err()
					return
				case line, ok := <-ch:
					if !ok {
						matched <- ErrNotRunning
						return
					}
					if re.MatchString(line.Text) {
						matched <- nil
						return
					}
				}
			}
		}()
	}
	for _, c := range hook.GetCommands() {
		if err := m.WriteInput(c); err != nil {
			return err
		}
	}
	if re == nil {
		return nil
	}
	if err := <-matched; err != nil {
		return fmt.Errorf("waiting for %q: %w", hook.GetWaitFor(), err)
	}
	return nil
}

// Done returns a channel closed when the current run exits. If nothing is
// running it returns a closed channel.
func (m *Manager) Done() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.done == nil || m.procState == supervisorv1.ProcessState_PROCESS_STATE_STOPPED {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return m.done
}
