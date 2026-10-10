// Package toolexec is a minimal in-process tool executor. It exists only to make the
// policy-enforcement acceptance test (SEC-001/TOOL-001) provable end-to-end before real tool
// implementations (RAG-001 retrieval, TOOL-002 MCP adapter) exist: a denied tool call must never
// reach this executor at all.
package toolexec

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aeon-ai/aeon/go/internal/sandbox"
)

// ExecuteFunc is what running a tool actually does. Real implementations (MCP-backed, sandboxed,
// etc.) will replace these — see roadmap.md RAG-001/TOOL-002, both still TODO.
//
// THE TENANT IS THE FIRST ARGUMENT AND NOT PART OF `args`, which is the whole point of the
// signature change GOV-001g made. `args` comes off the wire, so a tenant in there would be the
// caller choosing its own isolation boundary — the defect SEC-005 and the Memory Store each paid
// for once. As a parameter it is supplied by the gateway from `effectiveTenant`, and every tool
// author sees it and has to decide whether their tool is tenant-scoped. Five of the six ignore it;
// the sixth was reading other tenants' data before this existed.
type ExecuteFunc func(tenant string, args map[string]any) (map[string]any, error)

// shellExecImage is the image every shell.exec call runs in (TOOL-003) — small, fast to pull, and
// what go/internal/sandbox's own tests already exercise.
const shellExecImage = "alpine:3.20"

// Door names which entrance a call came through. It is recorded with every invocation because the
// defect INT-013 fixes was that the RECORD DEPENDED ON THE DOOR: a call carrying an idempotency key
// left a durable row in `tool_executions`, and the same call without one left nothing at all. An
// audit that cannot say which door a call used also cannot notice that a new door records nothing.
const (
	DoorHTTP       = "http"
	DoorHTTPDedupe = "http-dedupe"
	DoorMCP        = "mcp"
)

// Invocation is one tool call as a gateway hands it over.
//
// ATTRIBUTION TRAVELS WITH THE CALL, and that is the whole reason this is a struct rather than more
// positional parameters. Recording inside Execute is what makes the bitácora unforgettable, but
// Execute only ever knew the tenant, the name and the args — so a record written there could say
// "something called search.web" and never "which run, which agent". The fields a door knows have to
// reach the place that writes the row, or the coverage is bought by losing the attribution that is
// the point (Veritium's ask is literally "qué agente puede llamar cuál").
//
// Empty is a real answer here, not a missing one: an external MCP caller is not a run (INT-003), so
// RunID/StepID are empty for that door and the row says so instead of inventing an attribution.
type Invocation struct {
	Tenant   string
	ToolName string
	Args     map[string]any

	RunID            string
	StepID           string
	AgentManifestRef string
	Door             string
}

// InvocationRecord is what Execute asks the recorder to append. Defined here, in primitives, so
// toolexec does not import the store (and therefore pgx): the binary wires an adapter that
// translates this into store.ToolInvocation. The dependency direction matters — a tool executor
// that needs Postgres to be constructible stops being testable without it.
type InvocationRecord struct {
	Tenant           string
	ToolName         string
	Door             string
	Outcome          string // OutcomeOK | OutcomeError
	ErrorMessage     string
	RunID            string
	StepID           string
	AgentManifestRef string
	DurationMS       int64
}

// The two outcomes Execute can produce. A denial is not among them because a denial never reaches
// Execute — the policy check is strictly before it, which is the separation SEC-001's acceptance
// test depends on.
const (
	OutcomeOK    = "ok"
	OutcomeError = "error"
)

// Recorder appends the fact that a tool ran. A func rather than an interface because there is one
// implementation and it is an adapter, and because a test double is then a closure.
type Recorder func(ctx context.Context, rec InvocationRecord) error

// Executor dispatches a tool call by name.
type Executor struct {
	fns      map[string]ExecuteFunc
	recorder Recorder
}

// Outcome is what a door gets back.
//
// Recorded is surfaced rather than swallowed because "it ran and we failed to write it down" is the
// one fact an operator needs from an audit system, and because this gateway already answers exactly
// that way on the dedupe path: when the effect landed but the result could not be recorded, it
// returns the result plus a warning naming the exposure instead of failing. Same choice here, for
// the same reason — failing the call would turn an audit outage into a tool outage, and retrying
// would re-run the effect.
type Outcome struct {
	Result    map[string]any
	Recorded  bool
	RecordErr error
}

// WithRecorder returns the executor with a bitácora attached (INT-013). A nil recorder is allowed
// and means "record nothing", which is what every unit test that is not about recording uses.
func (e *Executor) WithRecorder(rec Recorder) *Executor {
	e.recorder = rec
	return e
}

// NewExecutor returns an executor seeded with the tools that need no deployment configuration.
//
// search.web is NOT among them, and that is TOOL-007's change. It used to be registered here as a
// function that echoed its own arguments back, so a deployment with no search provider still
// answered `{"status": "executed", ...}` and a deep-research run retrieved nothing while reporting
// success. It is now registered only by RegisterWebSearchTool, only when a provider is configured —
// the same rule TOOL-006 applied to search.rag, for the same reason: an absent tool fails loudly on
// the first call, and a lying one never fails at all.
//
// Tests that need a permitted tool which executes register their own double explicitly. That keeps
// the double visible in the test that relies on it instead of shipping it in the binary.
func NewExecutor() *Executor {
	e := &Executor{fns: map[string]ExecuteFunc{}}
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
	e.Register("shell.exec", func(_ string, args map[string]any) (map[string]any, error) {
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

// Execute runs a registered tool and records that it did. Callers MUST check
// policy.Engine.IsAllowed before calling this — Execute itself does not check authorization; that
// separation is what makes the policy check testable independently of tool implementations.
//
// THE RECORD IS WRITTEN HERE AND NOT BY THE DOORS, which is the design decision of INT-013 and not
// an implementation detail. There are three doors today (two HTTP, one MCP) and recording at each
// one is three chances to forget plus one more for every door added later; the measured defect was
// exactly that shape. Execute is the only way to run a tool, so a door that executes is a door that
// records, by construction rather than by review — the same reasoning that gave artifact.read a
// per-tenant os.Root instead of a check somebody has to remember.
//
// AN UNKNOWN TOOL IS NOT RECORDED. Nothing ran, so there is no invocation; the row would assert an
// execution that never happened. The caller still gets the error.
func (e *Executor) Execute(ctx context.Context, inv Invocation) (Outcome, error) {
	fn, ok := e.fns[inv.ToolName]
	if !ok {
		return Outcome{}, fmt.Errorf("toolexec: unknown tool %q", inv.ToolName)
	}

	started := time.Now()
	result, execErr := fn(inv.Tenant, inv.Args)
	elapsed := time.Since(started)

	out := Outcome{Result: result}
	if e.recorder == nil {
		return out, execErr
	}

	rec := InvocationRecord{
		Tenant: inv.Tenant, ToolName: inv.ToolName, Door: inv.Door,
		Outcome: OutcomeOK, RunID: inv.RunID, StepID: inv.StepID,
		AgentManifestRef: inv.AgentManifestRef, DurationMS: elapsed.Milliseconds(),
	}
	if execErr != nil {
		rec.Outcome, rec.ErrorMessage = OutcomeError, execErr.Error()
	}

	// WithoutCancel, with its own deadline: a client that hangs up after its tool ran must not erase
	// the record of the thing that ran. Dropping cancellation keeps the trace context (so the write
	// stays on the same span tree) while making the caller's disconnect stop deciding whether the
	// execution is auditable. The deadline is what keeps that from becoming an unbounded wait.
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := e.recorder(recCtx, rec); err != nil {
		out.RecordErr = err
		return out, execErr
	}
	out.Recorded = true
	return out, execErr
}

// recordTimeout bounds the bitácora write. Short on purpose: the execution already happened, so a
// slow audit write must not extend the caller's request much beyond it.
const recordTimeout = 5 * time.Second
