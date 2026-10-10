package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// Catalog keeps a live MCP server's tool list in step with the Tool Registry (INT-003).
//
// WHAT IT FIXES. aeon-toolgw read the registry ONCE at startup, so a tool registered or changed in
// aeon-controlplane afterwards stayed invisible until someone restarted the gateway — verified by hand
// before this existed: seeing a newly registered tool needed a real `docker compose restart toolgw`.
//
// AND A SECOND DEFECT THE REFRESH WOULD HAVE FAITHFULLY PRESERVED. ToolRegistry.List returns EVERY
// VERSION of every tool, ordered created_at DESC, and the old startup loop called AddTool once per row —
// so for a tool with two versions the newest was registered first and then OVERWRITTEN by the oldest.
// Measured before fixing: an external MCP client was shown the description and input schema of the
// FIRST version ever registered. The tool was present and callable with a stale schema, which is the
// worst shape of wrong: a client validating its arguments against it sends what an old version wanted.
type Catalog struct {
	server   *sdkmcp.Server
	eng      *policy.Engine
	executor ToolExecutor

	mu sync.Mutex
	// registered maps a tool name to the fingerprint of what is currently registered under it. The
	// fingerprint is what makes an unchanged catalogue produce NO notification: the SDK's AddTool
	// notifies `notifications/tools/list_changed` on every call, so re-adding all N tools on every poll
	// would tell every connected client the list changed every interval when nothing did — and a signal
	// that fires when nothing happened stops being read.
	registered map[string]string
}

// ToolExecutor is the subset of toolexec.Executor the handlers need.
//
// An interface rather than the concrete type so a test can supply a double without a real executor, and
// so this file does not widen what a catalogue is allowed to do to the tools it lists.
type ToolExecutor interface {
	// The tenant is the first argument since GOV-001g. This package passes "" because an external
	// MCP caller (INT-003) shares one Cedar principal and is not a run, so there is no run tenant —
	// and the tenant-scoped tools refuse an empty one rather than fall back to a deployment-wide
	// store, which is the behaviour that let a caller read artifacts somebody else's run produced.
	Execute(ctx context.Context, inv toolexec.Invocation) (toolexec.Outcome, error)
	// RecordRefusal is in this interface deliberately: a thing that can execute a tool must also be
	// able to record that one was refused (INT-014). Stating it here is stronger than a lint for
	// this door — a replacement executor that only knows how to run things does not compile — and
	// it is why the test double in catalog_test.go has to implement it too.
	RecordRefusal(ctx context.Context, inv toolexec.Invocation, disposition, policyID string) toolexec.Outcome
}

// CatalogChange reports what one Apply did. Returned rather than logged from inside, because the caller
// knows whether this is the first load (where "added 213" is expected) or a refresh (where it is news).
type CatalogChange struct {
	Added   []string
	Updated []string
	Removed []string
}

// Empty reports whether nothing changed.
func (c CatalogChange) Empty() bool {
	return len(c.Added) == 0 && len(c.Updated) == 0 && len(c.Removed) == 0
}

// NewCatalog builds a catalogue over an already-constructed MCP server.
func NewCatalog(server *sdkmcp.Server, eng *policy.Engine, executor ToolExecutor) *Catalog {
	return &Catalog{server: server, eng: eng, executor: executor, registered: map[string]string{}}
}

// CurrentVersions picks the newest record for each tool name.
//
// BY CreatedAt AND NOT BY Version, deliberately. `version` is a free-text column whose format Aeon does
// not own — a registry holding "10.0.0" and "9.0.0" would order them wrong under any string comparison,
// and parsing semver we do not enforce would be inventing a guarantee. When the platform wants
// version-ordered selection it needs a validated version format first; until then "most recently
// registered" is a fact the database actually knows.
//
// A pure function over the slice rather than a SQL clause, so it does not depend on the caller having
// asked for a particular ORDER BY — which is exactly the assumption the old startup loop made and got
// backwards.
func CurrentVersions(tools []*store.ToolRecord) []*store.ToolRecord {
	newest := map[string]*store.ToolRecord{}
	for _, rec := range tools {
		if rec == nil || rec.Name == "" {
			continue
		}
		prev, ok := newest[rec.Name]
		if !ok || rec.CreatedAt.After(prev.CreatedAt) {
			newest[rec.Name] = rec
		}
	}
	out := make([]*store.ToolRecord, 0, len(newest))
	for _, rec := range newest {
		out = append(out, rec)
	}
	return out
}

// Apply brings the server's tool list in line with tools, adding, replacing and removing only what
// actually differs.
func (c *Catalog) Apply(tools []*store.ToolRecord) CatalogChange {
	current := CurrentVersions(tools)

	c.mu.Lock()
	defer c.mu.Unlock()

	var change CatalogChange
	seen := make(map[string]struct{}, len(current))
	for _, rec := range current {
		seen[rec.Name] = struct{}{}
		fp := fingerprint(rec)
		existing, known := c.registered[rec.Name]
		if known && existing == fp {
			continue
		}
		schema, _ := rec.Descriptor["input_schema"].(map[string]any)
		if schema == nil {
			// AddTool needs a non-nil object schema. A registry row without one should not exist —
			// tool_descriptor.schema.json requires input_schema — so this stays defensive rather than
			// panicking on a malformed row, and "accept anything" is the only safe fallback for a
			// SCHEMA (the policy check is what protects the call, not this).
			schema = map[string]any{"type": "object"}
		}
		description, _ := rec.Descriptor["description"].(string)
		c.server.AddTool(
			&sdkmcp.Tool{Name: rec.Name, Description: description, InputSchema: schema},
			toolCallHandler(rec.Name, c.eng, c.executor),
		)
		c.registered[rec.Name] = fp
		if known {
			change.Updated = append(change.Updated, rec.Name)
		} else {
			change.Added = append(change.Added, rec.Name)
		}
	}

	for name := range c.registered {
		if _, still := seen[name]; still {
			continue
		}
		c.server.RemoveTools(name)
		delete(c.registered, name)
		change.Removed = append(change.Removed, name)
	}
	return change
}

// ToolLister is what a Catalog reads from. Narrower than *store.ToolRegistry so the poll loop is
// testable without Postgres, and so the catalogue cannot write to the registry it is watching.
type ToolLister interface {
	List(ctx context.Context) ([]*store.ToolRecord, error)
}

// MergeListers reads several listers as one, which is how TOOL-010's federated sources reach
// `tools/list` without this package writing to the Tool Registry: Postgres rows and federated rows
// meet here, in the catalogue, rather than in the database.
//
// ANY FAILURE FAILS THE WHOLE READ, on purpose. Returning the listers that did answer would be a
// partial catalogue, and Apply treats a tool's absence as "removed" — so one source's blip would
// withdraw its tools from every connected client. Failing instead lets Watch's own rule apply: a
// failed read changes nothing and the previous catalogue stands.
func MergeListers(listers ...ToolLister) ToolLister {
	return mergedLister(listers)
}

type mergedLister []ToolLister

func (m mergedLister) List(ctx context.Context) ([]*store.ToolRecord, error) {
	var all []*store.ToolRecord
	for _, l := range m {
		rows, err := l.List(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
	}
	return all, nil
}

// Watch polls lister every interval and applies what it finds, until ctx is done.
//
// A POLL AND NOT A DATABASE NOTIFICATION, and the reason is worth stating: the registry is written by a
// different process (aeon-controlplane), so the gateway would need LISTEN/NOTIFY plumbing and a
// reconnect story for a catalogue that changes when a human registers a tool. Polling a handful of rows
// on an interval is the proportionate mechanism, and the MCP clients are told through the protocol's own
// `notifications/tools/list_changed`, which the SDK emits from AddTool/RemoveTools.
//
// A FAILED READ CHANGES NOTHING. An error is logged and the previous catalogue stays exactly as it was:
// treating a read failure as "the registry is empty" would remove every tool and tell every connected
// client the catalogue is gone, turning a transient database blip into a fleet-wide outage of the thing
// this feature exists to keep fresh.
func (c *Catalog) Watch(ctx context.Context, lister ToolLister, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tools, err := lister.List(ctx)
			if err != nil {
				log.Printf("aeon-toolgw: refreshing the MCP catalog: %v (keeping the previous one)", err)
				continue
			}
			if change := c.Apply(tools); !change.Empty() {
				log.Printf("aeon-toolgw: MCP catalog refreshed — added=%v updated=%v removed=%v",
					change.Added, change.Updated, change.Removed)
			}
		}
	}
}

// fingerprint identifies what is registered for a tool name.
//
// Over the DESCRIPTOR and not just the version, because a registry row can be corrected in place: the
// same version with a fixed schema is a different tool to a client validating against it, and a
// version-only fingerprint would leave the old schema serving.
func fingerprint(rec *store.ToolRecord) string {
	raw, err := json.Marshal(rec.Descriptor)
	if err != nil {
		// Unhashable descriptor: return a value that never matches, so the tool is re-registered rather
		// than silently left stale. Failing toward "refresh it" is the right direction here.
		return "unhashable-" + rec.Version + "-" + rec.UpdatedAt.String()
	}
	sum := sha256.Sum256(append([]byte(rec.Version+"\x00"), raw...))
	return hex.EncodeToString(sum[:])
}
