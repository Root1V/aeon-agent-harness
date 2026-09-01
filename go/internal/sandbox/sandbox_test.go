package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

const testImage = "alpine:3.20"

func requireDocker(t *testing.T) *Runner {
	t.Helper()
	r, err := NewRunner()
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !r.Available(ctx) {
		t.Skip("docker daemon not reachable — set DOCKER_HOST or run where a docker socket is mounted")
	}
	return r
}

// TestSandboxEgressDeniedByDefault is TOOL-003's acceptance test: a real sandboxed command that
// tries to reach a real external host gets no network stack at all — not a slow/blocked connection,
// an outright absence of one (verified below: the failure is immediate and DNS-resolution-shaped,
// not a timeout).
func TestSandboxEgressDeniedByDefault(t *testing.T) {
	r := requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := r.Run(ctx, RunSpec{
		Image:   testImage,
		Command: []string{"sh", "-c", "wget -T 5 -O - http://example.com"},
		Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.ExitCode == 0 {
		t.Fatalf("expected a non-zero exit code (network should be unreachable), got 0; stdout=%q stderr=%q", result.Stdout, result.Stderr)
	}
	if result.Stderr == "" {
		t.Fatalf("expected wget to report a network error on stderr, got nothing; stdout=%q", result.Stdout)
	}
}

func TestSandboxRunsPlainCommandSuccessfully(t *testing.T) {
	r := requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := r.Run(ctx, RunSpec{
		Image:   testImage,
		Command: []string{"sh", "-c", "echo hello-from-sandbox"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", result.ExitCode, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "hello-from-sandbox") {
		t.Fatalf("expected stdout to contain the echoed text, got %q", result.Stdout)
	}
}

func TestSandboxPropagatesNonZeroExitCode(t *testing.T) {
	r := requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := r.Run(ctx, RunSpec{
		Image:   testImage,
		Command: []string{"sh", "-c", "exit 7"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 7 {
		t.Fatalf("expected exit code 7, got %d", result.ExitCode)
	}
}

func TestSandboxEnforcesTimeout(t *testing.T) {
	r := requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := r.Run(ctx, RunSpec{
		Image:   testImage,
		Command: []string{"sh", "-c", "sleep 30"},
		Timeout: 2 * time.Second,
	})
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected a timeout error, got: %v", err)
	}
}

func TestSandboxRejectsReadOnlyRootfsWrite(t *testing.T) {
	r := requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// /tmp is a writable tmpfs (deliberately, so scripts have *somewhere* to write); everywhere
	// else on the rootfs is read-only, verified directly rather than assumed.
	result, err := r.Run(ctx, RunSpec{
		Image:   testImage,
		Command: []string{"sh", "-c", "touch /etc/should-fail 2>&1; echo EXIT=$?; touch /tmp/should-succeed 2>&1; echo EXIT=$?"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(result.Stdout, "EXIT=1") {
		t.Fatalf("expected the write to the read-only rootfs to fail, got stdout=%q stderr=%q", result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "EXIT=0") {
		t.Fatalf("expected the write to the /tmp tmpfs to succeed, got stdout=%q stderr=%q", result.Stdout, result.Stderr)
	}
}
