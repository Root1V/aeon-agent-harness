package mcp

import (
	"context"
	"fmt"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// fakeExecutor stands in for toolexec.Executor: this file is about which tools are LISTED, not about
// what happens when one runs — that is TestToolGatewayServerEndToEndWithRealAdapter's job.
type fakeExecutor struct{}

func (fakeExecutor) Execute(_, _ string, _ map[string]any) (map[string]any, error) {
	return map[string]any{"status": "ok"}, nil
}

func permitEverything(t *testing.T) *policy.Engine {
	t.Helper()
	eng, err := policy.LoadEngine(policy.PolicyBundleDoc{Policies: []policy.PolicyBundleItem{
		{ID: "permit-all-for-catalog-tests", Effect: "permit",
			CedarSource: `permit(principal, action, resource);`},
	}})
	if err != nil {
		t.Fatalf("LoadEngine: %v", err)
	}
	return eng
}

// toolRecord builds a registry row. createdAt is explicit because version selection is BY CREATION TIME
// (see CurrentVersions) and a test that let both rows share a timestamp would not be testing anything.
func toolRecord(name, version, description string, createdAt time.Time) *store.ToolRecord {
	return &store.ToolRecord{
		ToolID: name + "." + version, Version: version, Name: name, CreatedAt: createdAt, UpdatedAt: createdAt,
		Descriptor: map[string]any{
			"description": description,
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{version: map[string]any{"type": "string"}},
			},
		},
	}
}

// listedTools connects a real in-memory MCP client and returns what tools/list actually reports.
//
// Through a real client session rather than by reading the Catalog's own map: the map is our bookkeeping
// and the protocol response is what a consumer sees, and the whole defect this file fixes was a
// difference between the two.
func listedTools(t *testing.T, server *sdkmcp.Server) map[string]string {
	t.Helper()
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "catalog-test", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	out := map[string]string{}
	for _, tool := range res.Tools {
		out[tool.Name] = tool.Description
	}
	return out
}

// TestMcpCatalogExposesTheNewestVersionOfEachTool pins the defect measured before this existed.
//
// ToolRegistry.List returns EVERY VERSION of every tool, and the old startup loop called AddTool once per
// row. List orders created_at DESC, so the newest was registered first and then overwritten by the
// oldest: an external MCP client was shown the description and input schema of the FIRST version ever
// registered. Measured, not suspected — the tool was present and callable with a stale schema, so a
// client validating its arguments against it would send what an old version wanted.
func TestMcpCatalogExposesTheNewestVersionOfEachTool(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now()

	t.Run("newest wins regardless of the order the rows arrive in", func(t *testing.T) {
		// Both orders, because the bug was an ORDER DEPENDENCE. A test that only fed one order would pass
		// against a function that simply took the last row it saw.
		for _, name := range []string{"newest first (List's own order)", "oldest first"} {
			rows := []*store.ToolRecord{
				toolRecord("search.web", "2.0.0", "NEWEST", recent),
				toolRecord("search.web", "1.0.0", "OLDEST", old),
			}
			if name == "oldest first" {
				rows[0], rows[1] = rows[1], rows[0]
			}
			t.Run(name, func(t *testing.T) {
				server, _ := NewToolGatewayCatalog(rows, permitEverything(t), fakeExecutor{})
				listed := listedTools(t, server)
				if len(listed) != 1 {
					t.Fatalf("listed %d tools, want 1 — two versions of one tool are one tool: %v", len(listed), listed)
				}
				if listed["search.web"] != "NEWEST" {
					t.Errorf("search.web description = %q, want %q — the catalogue is serving a stale schema",
						listed["search.web"], "NEWEST")
				}
			})
		}
	})

	t.Run("CurrentVersions does not depend on Version string ordering", func(t *testing.T) {
		// "10.0.0" sorts before "9.0.0" in any string comparison, and `version` is a free-text column
		// whose format Aeon does not enforce. Selection is by creation time precisely so this case is not
		// a trap — and this subtest is what would fail if someone "improved" it to compare versions.
		rows := []*store.ToolRecord{
			toolRecord("search.web", "9.0.0", "OLDER BUT HIGHER-SORTING", old),
			toolRecord("search.web", "10.0.0", "NEWEST", recent),
		}
		current := CurrentVersions(rows)
		if len(current) != 1 || current[0].Version != "10.0.0" {
			t.Fatalf("CurrentVersions = %+v, want only version 10.0.0", current)
		}
	})
}

// TestMcpCatalogRefreshesLive is INT-003's live-refresh acceptance test.
//
// Before this, aeon-toolgw read the registry once at start-up: a tool registered in aeon-controlplane
// afterwards stayed invisible until a real `docker compose restart toolgw`. The assertion is what a
// connected MCP client sees change, not what our own bookkeeping says.
func TestMcpCatalogRefreshesLive(t *testing.T) {
	now := time.Now()
	server, catalog := NewToolGatewayCatalog(
		[]*store.ToolRecord{toolRecord("search.web", "1.0.0", "v1", now)},
		permitEverything(t), fakeExecutor{},
	)

	if listed := listedTools(t, server); len(listed) != 1 || listed["search.web"] != "v1" {
		t.Fatalf("initial catalogue = %v", listed)
	}

	t.Run("a newly registered tool appears", func(t *testing.T) {
		change := catalog.Apply([]*store.ToolRecord{
			toolRecord("search.web", "1.0.0", "v1", now),
			toolRecord("search.rag", "1.0.0", "rag", now),
		})
		if len(change.Added) != 1 || change.Added[0] != "search.rag" {
			t.Fatalf("change = %+v, want exactly search.rag added", change)
		}
		if len(change.Updated) != 0 {
			t.Errorf("an unchanged tool was reported as updated: %v — every Updated entry sends a "+
				"list_changed notification to every connected client", change.Updated)
		}
		if listed := listedTools(t, server); listed["search.rag"] != "rag" {
			t.Errorf("search.rag is not listed after the refresh: %v", listed)
		}
	})

	t.Run("a NEW VERSION of an existing tool replaces it", func(t *testing.T) {
		change := catalog.Apply([]*store.ToolRecord{
			toolRecord("search.web", "2.0.0", "v2", now.Add(time.Hour)),
			toolRecord("search.rag", "1.0.0", "rag", now),
		})
		if len(change.Updated) != 1 || change.Updated[0] != "search.web" {
			t.Fatalf("change = %+v, want search.web updated", change)
		}
		if listed := listedTools(t, server); listed["search.web"] != "v2" {
			t.Errorf("search.web description = %q, want v2", listed["search.web"])
		}
	})

	t.Run("a descriptor corrected IN PLACE is noticed", func(t *testing.T) {
		// Same version, fixed schema. A fingerprint over the version alone would leave the broken schema
		// serving, which is worse than never refreshing: the operator fixed it and the gateway kept the
		// old one with no way to tell.
		corrected := toolRecord("search.web", "2.0.0", "v2 corrected", now.Add(time.Hour))
		change := catalog.Apply([]*store.ToolRecord{corrected, toolRecord("search.rag", "1.0.0", "rag", now)})
		if len(change.Updated) != 1 || change.Updated[0] != "search.web" {
			t.Fatalf("change = %+v, want search.web updated on an in-place descriptor fix", change)
		}
		if listed := listedTools(t, server); listed["search.web"] != "v2 corrected" {
			t.Errorf("description = %q, want the corrected one", listed["search.web"])
		}
	})

	t.Run("a retired tool disappears", func(t *testing.T) {
		change := catalog.Apply([]*store.ToolRecord{toolRecord("search.web", "2.0.0", "v2 corrected", now.Add(time.Hour))})
		if len(change.Removed) != 1 || change.Removed[0] != "search.rag" {
			t.Fatalf("change = %+v, want search.rag removed", change)
		}
		if listed := listedTools(t, server); len(listed) != 1 {
			t.Errorf("catalogue = %v, want only search.web", listed)
		}
	})

	t.Run("an UNCHANGED catalogue changes nothing at all", func(t *testing.T) {
		// The assertion that keeps the notification meaningful. The SDK's AddTool notifies
		// tools/list_changed on EVERY call, so re-adding all N tools each poll would tell every connected
		// client the list changed every interval when nothing did — and a signal that fires when nothing
		// happened stops being read.
		same := []*store.ToolRecord{toolRecord("search.web", "2.0.0", "v2 corrected", now.Add(time.Hour))}
		if change := catalog.Apply(same); !change.Empty() {
			t.Errorf("re-applying an identical catalogue reported %+v — every entry there is a spurious "+
				"list_changed notification", change)
		}
	})
}

// failingLister returns an error once, then a catalogue. It exists to prove the fail-safe.
type failingLister struct {
	calls int
	tools []*store.ToolRecord
}

func (f *failingLister) List(context.Context) ([]*store.ToolRecord, error) {
	f.calls++
	if f.calls == 1 {
		return nil, fmt.Errorf("simulated registry outage")
	}
	return f.tools, nil
}

// TestAFailedRefreshKeepsThePreviousCatalog is the fail-safe, and it is the most important test here.
//
// Treating a read failure as "the registry is empty" would remove every tool and tell every connected
// client the catalogue is gone — turning a transient database blip into a fleet-wide outage of the exact
// thing this feature exists to keep fresh. The direction of that error is the whole point: on failure the
// catalogue must go STALE, never EMPTY.
func TestAFailedRefreshKeepsThePreviousCatalog(t *testing.T) {
	now := time.Now()
	initial := []*store.ToolRecord{toolRecord("search.web", "1.0.0", "v1", now)}
	server, catalog := NewToolGatewayCatalog(initial, permitEverything(t), fakeExecutor{})

	lister := &failingLister{tools: []*store.ToolRecord{
		toolRecord("search.web", "1.0.0", "v1", now),
		toolRecord("search.rag", "1.0.0", "rag", now),
	}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		catalog.Watch(ctx, lister, 20*time.Millisecond)
		close(done)
	}()

	// Wait for the second poll: the first fails, the second succeeds.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if listed := listedTools(t, server); len(listed) == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if lister.calls < 2 {
		t.Fatalf("the watcher polled %d time(s) — it stopped after the failure instead of trying again", lister.calls)
	}
	listed := listedTools(t, server)
	if listed["search.web"] != "v1" {
		t.Errorf("search.web is gone or changed after a failed poll (%v) — a read error emptied the catalogue",
			listed)
	}
	if listed["search.rag"] != "rag" {
		t.Errorf("the catalogue never recovered after the failed poll: %v", listed)
	}
}

// TestWatchWithZeroIntervalDoesNothing: a disabled refresh must return, not spin.
func TestWatchWithZeroIntervalDoesNothing(t *testing.T) {
	_, catalog := NewToolGatewayCatalog(nil, permitEverything(t), fakeExecutor{})
	lister := &failingLister{}
	done := make(chan struct{})
	go func() {
		catalog.Watch(context.Background(), lister, 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch with a zero interval did not return — a disabled refresh is a goroutine leak")
	}
	if lister.calls != 0 {
		t.Errorf("a disabled refresh polled %d time(s)", lister.calls)
	}
}
