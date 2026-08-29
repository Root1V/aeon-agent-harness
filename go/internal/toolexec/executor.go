// Package toolexec is a minimal in-process tool executor. It exists only to make the
// policy-enforcement acceptance test (SEC-001/TOOL-001) provable end-to-end before real tool
// implementations (RAG-001 retrieval, TOOL-002 MCP adapter, TOOL-003 sandbox) exist: a denied tool
// call must never reach this executor at all.
package toolexec

import "fmt"

// ExecuteFunc is what running a tool actually does. Real implementations (MCP-backed, sandboxed,
// etc.) will replace these — see roadmap.md RAG-001/TOOL-002/TOOL-003, all still TODO.
type ExecuteFunc func(args map[string]any) (map[string]any, error)

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
	e.Register("shell.exec", func(args map[string]any) (map[string]any, error) {
		// This must never run in the reference deployment: policy_bundle.yaml forbids
		// "shell.*" for every agent. It exists so a policy regression is caught by actually
		// observing a real execution, not just by inspecting a decision object.
		return map[string]any{"status": "executed", "tool": "shell.exec", "args": args}, nil
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
