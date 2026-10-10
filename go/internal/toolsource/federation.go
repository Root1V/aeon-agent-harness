package toolsource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/gowebpki/jcs"
	"golang.org/x/oauth2/clientcredentials"

	aeonmcp "github.com/aeon-ai/aeon/go/internal/mcp"
	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// Federation keeps the approved tools of every source discoverable and callable.
//
// IT IS A ToolLister, which is how federated tools reach `tools/list` without this package writing
// to the Tool Registry. The catalogue (INT-003b) polls a lister and announces what it finds; the
// registry is written by aeon-controlplane and the catalogue's own doc says the lister "cannot write
// to the registry it is watching". Composing a lister keeps that true: Postgres rows and federated
// rows meet in the catalogue, not in the database.
type Federation struct {
	sources  []Source
	adapter  *aeonmcp.Adapter
	executor *toolexec.Executor

	mu       sync.Mutex
	sessions map[string]*aeonmcp.Session
	// rows is the last successful discovery per source. A SOURCE THAT FAILS KEEPS ITS PREVIOUS ROWS,
	// for the reason the catalogue gives for the same choice: treating a failed read as "this source
	// has no tools" would withdraw every one of them from every connected client and turn a network
	// blip into a fleet-wide outage of the thing this feature exists to provide.
	rows map[string][]*store.ToolRecord
}

// New builds a Federation over the sources in b. Nothing is connected until Refresh runs.
func New(b *Bundle, executor *toolexec.Executor) *Federation {
	return &Federation{
		sources:  b.Sources,
		adapter:  aeonmcp.NewAdapter(),
		executor: executor,
		sessions: map[string]*aeonmcp.Session{},
		rows:     map[string][]*store.ToolRecord{},
	}
}

// SourceReport is what one source's refresh did, and it is the operator's only view of the three
// ways a tool can fail to be served — which are deliberately distinguishable, because "I did not
// approve it", "it changed since I approved it" and "the source is down" need different actions.
type SourceReport struct {
	SourceID string
	Served   []string
	// NotApproved are tools the source offers that nobody listed. Not an error: the default is to
	// serve nothing, and a source adding a tool must not add it to Aeon by itself.
	NotApproved []string
	// Drifted names each approved tool whose live descriptor no longer matches the pinned digest,
	// with what was observed, so a review can end in a paste rather than in a guess.
	Drifted map[string]string
	// Missing are approved tools the source no longer offers at all.
	Missing []string
	Err     error
}

// Refresh re-lists every source and republishes what is approved and unchanged.
func (f *Federation) Refresh(ctx context.Context) []SourceReport {
	reports := make([]SourceReport, 0, len(f.sources))
	for i := range f.sources {
		reports = append(reports, f.refreshSource(ctx, &f.sources[i]))
	}
	return reports
}

func (f *Federation) refreshSource(ctx context.Context, s *Source) SourceReport {
	report := SourceReport{SourceID: s.ID, Drifted: map[string]string{}}

	session, err := f.session(ctx, s)
	if err != nil {
		report.Err = err
		return report
	}
	discovered, err := session.ListTools(ctx)
	if err != nil {
		// The session is dropped so the next refresh reconnects: a half-dead connection that keeps
		// answering errors would make this source permanently stale while looking configured.
		f.dropSession(s.ID)
		report.Err = err
		return report
	}

	offered := map[string]bool{}
	rows := make([]*store.ToolRecord, 0, len(discovered))
	for _, d := range discovered {
		offered[d.Name] = true
		approval, ok := s.Approval(d.Name)
		if !ok {
			report.NotApproved = append(report.NotApproved, d.Name)
			continue
		}
		digest, err := DescriptorFingerprint(d.Name, d.Description, d.InputSchema)
		if err != nil {
			report.Drifted[d.Name] = fmt.Sprintf("descriptor could not be canonicalized: %v", err)
			continue
		}
		if digest != approval.DescriptorSHA256 {
			// NOT SERVED, and that is the point of the pin. The classification the operator gave
			// describes the descriptor they read; this is a different one. Serving it anyway would
			// mean the approval covers whatever the source sends today.
			report.Drifted[d.Name] = digest
			continue
		}

		qualified := s.QualifiedName(d.Name)
		rows = append(rows, &store.ToolRecord{
			ToolID:     qualified,
			Version:    "1.0.0",
			Name:       qualified,
			SideEffect: approval.SideEffect,
			Risk:       approval.Risk,
			Descriptor: map[string]any{
				"description":  d.Description,
				"input_schema": d.InputSchema,
				// mcp_origin has been in tool_descriptor.schema.json since F0 with no producer and no
				// consumer; this is its first producer. See VRT-AEON-006, where it was reported as a
				// declared field nobody filled.
				"mcp_origin": map[string]any{
					"server":       s.Endpoint,
					"spec_version": session.NegotiatedProtocolVersion(),
				},
				"idempotency_key_fields": approval.IdempotencyKeyFields,
				"descriptor_sha256":      digest,
			},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		})
		report.Served = append(report.Served, qualified)
		f.registerExecutor(s, approval.Name, qualified)
	}

	for i := range s.Tools {
		if !offered[s.Tools[i].Name] {
			report.Missing = append(report.Missing, s.Tools[i].Name)
		}
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	f.mu.Lock()
	f.rows[s.ID] = rows
	f.mu.Unlock()
	return report
}

// registerExecutor makes the qualified name callable. Registering into the SAME executor every door
// already uses is what puts Cedar in front of a federated call without any new enforcement code:
// both the MCP door and the HTTP doors check policy and only then reach Execute, and INT-013/014
// record the invocation whichever way it is resolved.
//
// THE TENANT IS NOT FORWARDED, and an operator has to know this before approving a tool that reads
// data. `ExecuteFunc` receives Aeon's tenant for the call, but the request leaves with Aeon's own
// credential and the source has no notion of our tenants: agreed with Veritium in VRT-AEON-006
// (their quota sees Aeon as one client, and per-agent control is Cedar's job on our side). So a
// federated tool is governed per agent HERE and is not isolated per tenant THERE. For a tool that
// returns tenant-specific data, that isolation would have to exist at the source; approving such a
// tool without it would hand one tenant another's data through a door Cedar happily opened.
func (f *Federation) registerExecutor(s *Source, sourceToolName, qualified string) {
	endpointID := s.ID
	f.executor.Register(qualified, func(_ string, args map[string]any) (map[string]any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()

		session, err := f.sessionByID(ctx, endpointID)
		if err != nil {
			return nil, fmt.Errorf("toolsource: %s: %w", endpointID, err)
		}
		outcome, err := session.CallTool(ctx, sourceToolName, args)
		if err != nil {
			// A PROTOCOL-LEVEL failure (transport, unknown tool, bad arguments) is this call failing,
			// so the session is dropped and the error is returned — the adapter's doc is explicit that
			// a caller must not conflate it with a tool-level error.
			f.dropSession(endpointID)
			return nil, fmt.Errorf("toolsource: %s/%s: %w", endpointID, sourceToolName, err)
		}
		if outcome.IsError {
			// A TOOL-LEVEL error is a result to act on, not this call failing. Returned as an error
			// because that is how every other tool in toolexec reports a failed execution, and
			// INT-013 records it as outcome=error with this text.
			return nil, fmt.Errorf("toolsource: %s/%s reported an error: %s", endpointID, sourceToolName, outcome.Text)
		}
		return map[string]any{
			"source": endpointID,
			"tool":   sourceToolName,
			"text":   outcome.Text,
		}, nil
	})
}

// callTimeout bounds a federated call. A source that stops answering must not hold an Aeon worker
// open indefinitely; this is the one knob that keeps someone else's outage from becoming ours.
const callTimeout = 60 * time.Second

// List satisfies mcp.ToolLister: every source's last successful discovery, in a stable order.
func (f *Federation) List(context.Context) ([]*store.ToolRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*store.ToolRecord, 0, len(f.rows))
	for _, rows := range f.rows {
		out = append(out, rows...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Watch refreshes every source on its own interval until ctx is done, logging each report.
func (f *Federation) Watch(ctx context.Context) {
	for i := range f.sources {
		s := &f.sources[i]
		go func() {
			ticker := time.NewTicker(s.RefreshInterval())
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					logReport(f.refreshSource(ctx, s))
				}
			}
		}()
	}
}

// LogReports prints what a refresh did. The drift digests are printed on purpose: reviewing a
// changed descriptor and pasting its digest is the whole approval loop, and an operator who is only
// told "it drifted" has to go compute the hash themselves.
func LogReports(reports []SourceReport) {
	for _, r := range reports {
		logReport(r)
	}
}

func logReport(r SourceReport) {
	if r.Err != nil {
		log.Printf("aeon-toolgw: tool source %q unreachable, keeping its previous tools: %v", r.SourceID, r.Err)
		return
	}
	log.Printf("aeon-toolgw: tool source %q serving %d tool(s): %v", r.SourceID, len(r.Served), r.Served)
	if len(r.NotApproved) > 0 {
		log.Printf("aeon-toolgw: tool source %q offers %d tool(s) nobody approved, not served: %v",
			r.SourceID, len(r.NotApproved), r.NotApproved)
	}
	for name, observed := range r.Drifted {
		log.Printf("aeon-toolgw: tool source %q tool %q NOT SERVED: its descriptor changed since it was "+
			"approved. Review it and, if it is still acceptable, set descriptor_sha256: %s",
			r.SourceID, name, observed)
	}
	if len(r.Missing) > 0 {
		log.Printf("aeon-toolgw: tool source %q no longer offers approved tool(s): %v", r.SourceID, r.Missing)
	}
}

// DescriptorFingerprint hashes the part of a tool's descriptor an approval is about.
//
// RFC 8785 (JCS) and not encoding/json, and the same canonicalizer go/internal/stepidentity uses —
// agreed with the other teams in INT-011 for exactly this class of problem. A map marshals in Go's
// own key order for a struct but JSON objects from the wire are maps, so without canonicalization
// the digest would change when a source reordered its schema keys and every refresh would look like
// drift.
//
// NAME, DESCRIPTION AND INPUT SCHEMA, and nothing else: those are what a person reads to decide a
// side effect and a risk. Including a field an approval does not depend on would make a harmless
// change look like drift and train the operator to re-paste digests without reading.
func DescriptorFingerprint(name, description string, schema map[string]any) (string, error) {
	raw, err := json.Marshal(map[string]any{
		"name":         name,
		"description":  description,
		"input_schema": schema,
	})
	if err != nil {
		return "", fmt.Errorf("toolsource: encoding descriptor: %w", err)
	}
	canonical, err := jcs.Transform(raw)
	if err != nil {
		return "", fmt.Errorf("toolsource: canonicalizing descriptor: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func (f *Federation) session(ctx context.Context, s *Source) (*aeonmcp.Session, error) {
	f.mu.Lock()
	if existing, ok := f.sessions[s.ID]; ok {
		f.mu.Unlock()
		return existing, nil
	}
	f.mu.Unlock()

	session, err := f.adapter.ConnectWithClient(ctx, s.Endpoint, httpClientFor(s))
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.sessions[s.ID] = session
	f.mu.Unlock()
	return session, nil
}

func (f *Federation) sessionByID(ctx context.Context, id string) (*aeonmcp.Session, error) {
	for i := range f.sources {
		if f.sources[i].ID == id {
			return f.session(ctx, &f.sources[i])
		}
	}
	return nil, fmt.Errorf("no such tool source %q", id)
}

func (f *Federation) dropSession(id string) {
	f.mu.Lock()
	session := f.sessions[id]
	delete(f.sessions, id)
	f.mu.Unlock()
	if session != nil {
		_ = session.Close()
	}
}

// httpClientFor builds the client the MCP transport uses, with OAuth2 client-credentials when the
// source declares auth. nil means "the SDK's default client", which is correct for a source with no
// auth — a test server, or one behind a network boundary.
//
// The secrets are read from the environment HERE and not stored on the Source, so a Bundle that is
// logged or serialized anywhere cannot carry them.
func httpClientFor(s *Source) *http.Client {
	if s.Auth == nil {
		return nil
	}
	cfg := clientcredentials.Config{
		ClientID:     os.Getenv(s.Auth.ClientIDEnv),
		ClientSecret: os.Getenv(s.Auth.ClientSecretEnv),
		TokenURL:     s.Auth.TokenURL,
		Scopes:       s.Auth.Scopes,
	}
	return cfg.Client(context.Background())
}
