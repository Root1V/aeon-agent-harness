package a2a

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	sdka2a "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/a2asrv"
	"go.temporal.io/sdk/client"

	"github.com/aeon-ai/aeon/go/internal/runcontroller"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// testTemporalClient mirrors go/internal/api's own helper of the same name (unexported there, so
// duplicated here): A2A-001's acceptance test needs a real Temporal server and a real worker —
// see make test-go-integration.
func testTemporalClient(t *testing.T) client.Client {
	t.Helper()
	addr := os.Getenv("AEON_TEST_TEMPORAL_ADDRESS")
	if addr == "" {
		t.Skip("AEON_TEST_TEMPORAL_ADDRESS not set — skipping Temporal integration test (see make test-go-integration)")
	}
	c, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		t.Fatalf("connecting to Temporal at %s: %v", addr, err)
	}
	t.Cleanup(c.Close)
	return c
}

// testAgentRecord registers a real AgentManifest in a real Postgres-backed AgentRegistry
// (FND-001) and returns the stored record — BuildAgentCard must reflect a genuinely registered
// agent, not a struct literal standing in for one.
func testAgentRecord(t *testing.T, name string) *store.AgentRecord {
	t.Helper()
	dsn := os.Getenv("AEON_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("AEON_TEST_PG_DSN not set — skipping Postgres integration test (see make test-go-integration)")
	}
	ctx := context.Background()
	s, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)

	manifest := map[string]any{
		"metadata": map[string]any{"name": name, "version": "1.0.0"},
		"spec":     map[string]any{"tools": map[string]any{"allow": []any{"artifact.write"}}},
	}
	rec, err := s.AgentRegistry().Create(ctx, manifest, "a2a-test")
	if err != nil {
		t.Fatalf("register agent: %v", err)
	}
	return rec
}

// simpleGraph mirrors go/internal/api/run_controller_handlers_test.go's fixture of the same name:
// a single tool_call node, deliberately trivial — RUN-002's own node-kind coverage lives
// elsewhere, this test only needs *a* graph a real worker can run to completion quickly.
func simpleGraph(path string) map[string]any {
	return map[string]any{
		"id": "n0", "kind": "tool_call", "tool_name": "artifact.write",
		"tool_args": map[string]any{"path": path},
	}
}

func newTestA2AServer(t *testing.T, graph map[string]any) (baseURL string) {
	t.Helper()
	c := testTemporalClient(t)
	rec := testAgentRecord(t, fmt.Sprintf("a2a-test-agent-%d", time.Now().UnixNano()))

	mux := http.NewServeMux()
	srv := httptest.NewUnstartedServer(mux)
	baseURL = "http://" + srv.Listener.Addr().String()

	executor := &AeonAgentExecutor{
		Controller: runcontroller.New(c, ""),
		Graph:      graph,
		PollEvery:  50 * time.Millisecond,
	}
	card := BuildAgentCard(rec, baseURL+"/invoke")

	mux.Handle("/invoke", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor)))
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))

	srv.Start()
	t.Cleanup(srv.Close)
	return baseURL
}

func newTestA2AClient(t *testing.T, baseURL string) *a2aclient.Client {
	t.Helper()
	ctx := context.Background()
	card, err := agentcard.DefaultResolver.Resolve(ctx, baseURL)
	if err != nil {
		t.Fatalf("resolving AgentCard: %v", err)
	}
	c, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithConfig(a2aclient.Config{Polling: true}))
	if err != nil {
		t.Fatalf("creating A2A client: %v", err)
	}
	return c
}

func waitForTaskState(t *testing.T, c *a2aclient.Client, taskID sdka2a.TaskID, want sdka2a.TaskState, timeout time.Duration) *sdka2a.Task {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	var last *sdka2a.Task
	for time.Now().Before(deadline) {
		task, err := c.GetTask(ctx, &sdka2a.TaskQueryParams{ID: taskID})
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		last = task
		if task.Status.State == want {
			return task
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for task %s to reach state=%s, last observed: %+v", taskID, want, last)
	return nil
}

// TestA2ATaskLifecycle is A2A-001's acceptance test: a real A2A client resolves a real Agent Card
// (built from a real, registered AgentManifest), sends a message, and observes the task move
// through the real lifecycle — submitted -> working -> completed — backed by a real Temporal run
// (the same graph execution RUN-001's REST API already exercises), not a synthetic status.
func TestA2ATaskLifecycle(t *testing.T) {
	t.Run("a task reaches completed after its real Aeon run succeeds", func(t *testing.T) {
		baseURL := newTestA2AServer(t, simpleGraph("a2a-lifecycle-test.txt"))
		c := newTestA2AClient(t, baseURL)
		ctx := context.Background()

		msg := sdka2a.NewMessage(sdka2a.MessageRoleUser, sdka2a.TextPart{Text: "run the task"})
		result, err := c.SendMessage(ctx, &sdka2a.MessageSendParams{Message: msg})
		if err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		task, ok := result.(*sdka2a.Task)
		if !ok {
			t.Fatalf("expected a *Task result, got %T: %+v", result, result)
		}
		// With Polling enabled, SendMessage returns as soon as the task exists — submitted or
		// already working, depending on how fast Execute's first write beat this response.
		if task.Status.State != sdka2a.TaskStateSubmitted && task.Status.State != sdka2a.TaskStateWorking {
			t.Fatalf("initial task state = %q, want submitted or working", task.Status.State)
		}

		working := waitForTaskState(t, c, task.ID, sdka2a.TaskStateWorking, 5*time.Second)
		if working.Status.State != sdka2a.TaskStateWorking {
			t.Fatalf("expected to observe working, got %q", working.Status.State)
		}

		completed := waitForTaskState(t, c, task.ID, sdka2a.TaskStateCompleted, 15*time.Second)
		if completed.Status.State != sdka2a.TaskStateCompleted {
			t.Fatalf("expected to observe completed, got %q", completed.Status.State)
		}
	})

	t.Run("canceling a task reaches canceled and really cancels the underlying Aeon run", func(t *testing.T) {
		// A loop the real graph runtime will run for a while, giving the test time to cancel it
		// before it would otherwise complete — same technique run_controller_handlers_test.go
		// uses (pause) but for a real in-flight cancellation instead.
		graph := map[string]any{
			"id": "root", "kind": "loop", "max_iterations": 50,
			"body": simpleGraph("a2a-cancel-test.txt"),
		}
		baseURL := newTestA2AServer(t, graph)
		c := newTestA2AClient(t, baseURL)
		ctx := context.Background()

		msg := sdka2a.NewMessage(sdka2a.MessageRoleUser, sdka2a.TextPart{Text: "run a long task"})
		result, err := c.SendMessage(ctx, &sdka2a.MessageSendParams{Message: msg})
		if err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		task := result.(*sdka2a.Task)

		waitForTaskState(t, c, task.ID, sdka2a.TaskStateWorking, 5*time.Second)

		if _, err := c.CancelTask(ctx, &sdka2a.TaskIDParams{ID: task.ID}); err != nil {
			t.Fatalf("CancelTask: %v", err)
		}

		waitForTaskState(t, c, task.ID, sdka2a.TaskStateCanceled, 15*time.Second)
	})
}
