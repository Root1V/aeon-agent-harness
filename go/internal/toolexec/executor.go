// Package toolexec is a minimal in-process tool executor. It exists only to make the
// policy-enforcement acceptance test (SEC-001/TOOL-001) provable end-to-end before real tool
// implementations (RAG-001 retrieval, TOOL-002 MCP adapter) exist: a denied tool call must never
// reach this executor at all.
package toolexec

import (
	"context"
	"fmt"
	"sync"

	"github.com/aeon-ai/aeon/go/internal/sandbox"
)

// ExecuteFunc is what running a tool actually does. Real implementations (MCP-backed, sandboxed,
// etc.) will replace these — see roadmap.md RAG-001/TOOL-002, both still TODO.
type ExecuteFunc func(args map[string]any) (map[string]any, error)

// shellExecImage is the image every shell.exec call runs in (TOOL-003) — small, fast to pull, and
// what go/internal/sandbox's own tests already exercise.
const shellExecImage = "alpine:3.20"

// Executor dispatches a tool call by name.
type Executor struct {
	fns map[string]ExecuteFunc
}

// NewExecutor returns an executor seeded with a couple of demonstration tools matching
// examples/deep-research/agent.yaml's allow list (search.web) and policy_bundle.yaml's explicit
// forbids (shell.exec) — enough to prove an allowed call runs and a denied one never does.
func NewExecutor() *Executor {
	e := &Executor{fns: map[string]ExecuteFunc{}}
	e.Register("search.web", func(args map[string]any) (map[string]any, error) {
		return map[string]any{"status": "executed", "tool": "search.web", "args": args}, nil
	})
	// A real sandboxed shell (TOOL-003), not a fake stand-in — but this must still never run in
	// the reference deployment: policy_bundle.yaml forbids "shell.*" for every agent. It exists so
	// a policy regression is caught by actually observing a real (sandboxed) execution, not just
	// by inspecting a decision object. The Docker connection is established lazily, on first call,
	// so gateway startup never depends on Docker being reachable.
	var (
		once      sync.Once
		runner    *sandbox.Runner
		runnerErr error
	)
	e.Register("shell.exec", func(args map[string]any) (map[string]any, error) {
		command, ok := args["command"].(string)
		if !ok || command == "" {
			return nil, fmt.Errorf("toolexec: shell.exec: missing required string arg %q", "command")
		}
		once.Do(func() { runner, runnerErr = sandbox.NewRunner() })
		if runnerErr != nil {
			return nil, fmt.Errorf("toolexec: shell.exec: %w", runnerErr)
		}
		result, err := runner.Run(context.Background(), sandbox.RunSpec{Image: shellExecImage, Command: []string{"sh", "-c", command}})
		if err != nil {
			return nil, fmt.Errorf("toolexec: shell.exec: %w", err)
		}
		return map[string]any{
			"status":    "executed",
			"tool":      "shell.exec",
			"exit_code": result.ExitCode,
			"stdout":    result.Stdout,
			"stderr":    result.Stderr,
		}, nil
	})
	return e
}

// Register adds or replaces a tool's execution function.
func (e *Executor) Register(toolName string, fn ExecuteFunc) {
	e.fns[toolName] = fn
}

// Execute runs a registered tool. Callers MUST check policy.Engine.IsAllowed before calling this —
// Execute itself does not check authorization; that separation is what makes the policy check
// testable independently of tool implementations.
func (e *Executor) Execute(toolName string, args map[string]any) (map[string]any, error) {
	fn, ok := e.fns[toolName]
	if !ok {
		return nil, fmt.Errorf("toolexec: unknown tool %q", toolName)
	}
	return fn(args)
}
