// Package sandbox runs real, isolated shell commands for TOOL-003 — the "shell/code/browser"
// execution engine every action-oriented tool (e.g. shell.exec) will use once a profile's Cedar
// policy allows it (none does yet; see backlog.md). It replaces toolexec's fake shell.exec stand-in
// (go/internal/toolexec/executor.go), which never actually ran anything.
//
// Isolation engine: real Docker containers via the official Engine API client
// (github.com/moby/moby/client), not gVisor/Firecracker microVMs as the architecture doc's original
// wording ("Sandbox microVM/gVisor") names. gVisor's runsc needs a Linux host with ptrace/KVM
// support; Firecracker needs KVM directly — neither is available through Docker Desktop's
// virtualized backend on this project's macOS development machine, and requiring one would make
// this feature untestable here. Docker's own container isolation (namespaces + cgroups + capability
// dropping + a read-only rootfs) is real, non-simulated, kernel-enforced isolation — a documented,
// honest substitution, not a mock (see backlog.md's entry on hardening this to a true microVM
// runtime later).
//
// Egress: default-deny only, via NetworkMode("none") — a container created this way gets no network
// stack at all, not merely a restricted one; a DNS lookup fails outright (verified: busybox wget
// reports "bad address" instantly, no timeout). A configurable per-call egress *allowlist* (the
// architecture doc's other stated requirement) is not implemented — see backlog.md; every sandboxed
// command runs with zero network access, full stop.
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const (
	defaultTimeout  = 30 * time.Second
	defaultMemoryMB = 256
)

// RunSpec describes one sandboxed command execution.
type RunSpec struct {
	// Image is the container image the command runs in (e.g. "alpine:3.20"). Must already be
	// pullable — Runner pulls it if not already present locally.
	Image string
	// Command is passed as Config.Cmd — e.g. []string{"sh", "-c", "echo hi"}.
	Command []string
	// Timeout bounds the whole run (pull excluded); defaults to 30s.
	Timeout time.Duration
	// MemoryMB caps the container's memory; defaults to 256.
	MemoryMB int64
}

// RunResult is one sandboxed command's outcome.
type RunResult struct {
	ExitCode int64
	Stdout   string
	Stderr   string
}

// Runner executes RunSpecs against a real Docker daemon.
type Runner struct {
	cli *client.Client
}

// NewRunner connects to the Docker daemon the environment points at (DOCKER_HOST, or the default
// socket) — the same discovery `docker` the CLI itself uses.
func NewRunner() (*Runner, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("sandbox: connecting to docker: %w", err)
	}
	return &Runner{cli: cli}, nil
}

// Available reports whether a Docker daemon is actually reachable — callers (and this package's own
// tests) use this to fail fast/skip with a clear reason instead of a confusing timeout.
func (r *Runner) Available(ctx context.Context) bool {
	_, err := r.cli.Ping(ctx, client.PingOptions{})
	return err == nil
}

func (r *Runner) ensureImage(ctx context.Context, ref string) error {
	if _, err := r.cli.ImageInspect(ctx, ref); err == nil {
		return nil
	}
	resp, err := r.cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("sandbox: pulling image %s: %w", ref, err)
	}
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("sandbox: pulling image %s: %w", ref, err)
	}
	return nil
}

// Run executes one command in a freshly-created, freshly-removed container: no network stack
// (NetworkMode "none" — see package doc), a read-only root filesystem plus a small writable tmpfs at
// /tmp, every Linux capability dropped, and no-new-privileges. The container is always removed
// afterward, success or failure.
func (r *Runner) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	memoryMB := spec.MemoryMB
	if memoryMB <= 0 {
		memoryMB = defaultMemoryMB
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := r.ensureImage(runCtx, spec.Image); err != nil {
		return RunResult{}, err
	}

	created, err := r.cli.ContainerCreate(runCtx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: spec.Image,
			Cmd:   spec.Command,
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    container.NetworkMode("none"),
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			Tmpfs:          map[string]string{"/tmp": "size=64m"},
			Resources:      container.Resources{Memory: memoryMB * 1024 * 1024},
		},
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("sandbox: creating container: %w", err)
	}
	// Force-removed with a fresh (non-cancelable) context: runCtx may already be past its
	// deadline by the time we get here (a slow/hung command), and a cancelled context would make
	// cleanup itself fail, leaking the container.
	defer func() {
		_, _ = r.cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	}()

	// Condition must be NextExit, not the (default) NotRunning: a just-created, not-yet-started
	// container is already "not running", so NotRunning fires immediately with a meaningless
	// zero-value StatusCode instead of waiting for the command to actually finish. Found by
	// running this for real: every exit code came back 0 regardless of the command's real exit
	// status until switching to NextExit. Set up before Start (per the client's own documented
	// usage) so no exit can be missed between Start and Wait.
	waitResult := r.cli.ContainerWait(runCtx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})

	if _, err := r.cli.ContainerStart(runCtx, created.ID, client.ContainerStartOptions{}); err != nil {
		return RunResult{}, fmt.Errorf("sandbox: starting container: %w", err)
	}

	// Follow:true and reading to EOF is what makes this race-free: the log stream only closes once
	// the container has genuinely exited and the daemon has flushed it, so draining it fully *is*
	// the real synchronization point — a one-shot (non-following) log fetch right after a wait
	// signal can race the daemon's own log flush and silently truncate output.
	logs, err := r.cli.ContainerLogs(runCtx, created.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
	if err != nil {
		return RunResult{}, fmt.Errorf("sandbox: fetching logs: %w", err)
	}
	var stdout, stderr bytes.Buffer
	_, copyErr := stdcopy.StdCopy(&stdout, &stderr, logs)
	logs.Close()
	if copyErr != nil {
		if runCtx.Err() != nil {
			return RunResult{}, fmt.Errorf("sandbox: timed out after %s", timeout)
		}
		return RunResult{}, fmt.Errorf("sandbox: reading logs: %w", copyErr)
	}

	var exitCode int64
	select {
	case res := <-waitResult.Result:
		exitCode = res.StatusCode
	case err := <-waitResult.Error:
		return RunResult{}, fmt.Errorf("sandbox: waiting for container: %w", err)
	case <-runCtx.Done():
		return RunResult{}, fmt.Errorf("sandbox: timed out after %s", timeout)
	}

	return RunResult{ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}
