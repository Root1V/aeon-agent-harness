package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/aeon-ai/aeon/go/internal/a2a"
	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/secrets"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// delegationCredentialTTL is how long the lease for an injected credential lives.
//
// Short because the credential is used once, within this request, and immediately revoked. It goes
// through the Broker's real lease machinery rather than reading the secret straight out of it, so that
// A5's RevokeAllForOwner — the kill switch — covers a credential handed to a remote agent on behalf of a
// quarantined agent version, which reading the value directly would have left outside its reach.
const delegationCredentialTTL = 2 * time.Minute

// A2AEgressHandlers is A2A-002: governed A2A egress as a DATA-PLANE PROXY.
//
// Why a proxy and not the two designs that came before it. An authorize-API the framework consults is
// advisory by construction — a loop with a bug skips it and the effect still leaves. An A2A client of our
// own means the framework adopts our SDK and the task lifecycle moves to us, which is not ours to hold.
// A proxy on the path is a frontier: the framework points its A2A client at this endpoint, changes no
// code, and cannot reach the destination any other way. It is the third instance of a shape this codebase
// already has twice — INT-002's OpenAI-compatible endpoint and INT-003's outbound MCP server.
//
// THE LIMIT, stated because a frontier that is oversold is worse than none: this proxy governs an agent
// that cannot reach the network except through it. It does not AUTHENTICATE the calling process — the
// agent identity arrives in a header, exactly as it arrives in a body field on /execute today. Closing
// that is workload identity (SPIFFE), which is its own feature and not this one. What this does close is
// the hole Synaptum named on 2026-09-20: a governed agent could not do anything unauthorized itself, but
// could ASK A THIRD PARTY to, and the child's tools cross the child's gateway and not ours.
type A2AEgressHandlers struct {
	Policy       *policy.Engine
	RemoteAgents *store.RemoteAgents
	Delegations  *store.A2ADelegations
	Broker       *secrets.Broker
	// Client is the outbound HTTP client. Injectable so a test drives a real A2A server over a real
	// socket rather than a stubbed round-tripper.
	Client *http.Client
	// MaxInFlightPerRun is the FAN-OUT limit, and it lives here rather than in the framework because of
	// how the two halves of blast radius are split (settled with Synaptum on 2026-09-20): they hold
	// max_delegation_depth, because depth is visible to whoever knows the tree. Width is visible to
	// whoever is on the path of every call — fifty simultaneous delegations are fifty requests through
	// this one proxy from one identified process. Neither of us sees the whole thing alone.
	//
	// Zero means unlimited, which is a deployment's choice rather than a default we invent.
	MaxInFlightPerRun int
	// InFlightStaleAfter bounds which open rows still count. See store.OpenOptions.StaleAfter for what
	// this costs.
	InFlightStaleAfter time.Duration
}

// Register mounts the egress proxy. The remote agent id is in the PATH, not the body: the criterion is
// an explicit endpoint rather than network topology, because agent-to-agent delegation is usually
// east-west and never crosses an egress firewall — a rule that relied on the network would not fire.
func (h *A2AEgressHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /a2a/egress/{remote_agent_id}", h.proxy)
	mux.HandleFunc("GET /a2a/remote-agents", h.listRemoteAgents)
	mux.HandleFunc("POST /a2a/remote-agents", h.declareRemoteAgent)
}

func (h *A2AEgressHandlers) listRemoteAgents(w http.ResponseWriter, r *http.Request) {
	recs, err := h.RemoteAgents.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"remote_agents": recs})
}

func (h *A2AEgressHandlers) declareRemoteAgent(w http.ResponseWriter, r *http.Request) {
	var rec store.RemoteAgentRecord
	if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.RemoteAgents.Declare(r.Context(), rec)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// proxy is the whole governed path: identify, authorize, admit, inject, forward, classify, record.
func (h *A2AEgressHandlers) proxy(w http.ResponseWriter, r *http.Request) {
	remoteAgentID := r.PathValue("remote_agent_id")
	ctx, span := toolGatewayTracer.Start(r.Context(), "delegate_task", trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "delegate_task"),
		attribute.String("aeon.a2a.remote_agent_id", remoteAgentID),
	))
	defer span.End()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rpc, err := a2a.ParseRPCRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	rec := store.Delegation{
		RunID:            r.Header.Get(a2a.HeaderRunID),
		StepID:           r.Header.Get(a2a.HeaderStepID),
		AgentManifestRef: r.Header.Get(a2a.HeaderAgentManifestRef),
		RemoteAgentID:    remoteAgentID,
		RPCMethod:        rpc.Method,
		HopDepth:         hopDepth(r.Header.Get(a2a.HeaderHopDepth)),
	}
	span.SetAttributes(
		attribute.String("aeon.a2a.rpc_method", rpc.Method),
		attribute.String("gen_ai.agent.name", rec.AgentManifestRef),
	)

	// No identity, no delegation. Refused rather than proxied unattributed: an unattributed call through
	// this proxy would be a way around the per-agent policy every other path obeys, which is the failure
	// this endpoint exists to prevent rather than one it may introduce.
	if rec.AgentManifestRef == "" {
		h.deny(ctx, w, span, rec, http.StatusForbidden,
			"no "+a2a.HeaderAgentManifestRef+" on the request: a delegation with no agent identity cannot be authorized, and proxying it unattributed would be a way around the policy every other path obeys")
		return
	}

	// Declared before delegable. Separate from the policy decision below, and the order matters for the
	// message: "nobody declared this destination" is a missing registry entry and "policy refused it" is
	// a decision, fixed in different places by different people.
	remote, err := h.RemoteAgents.Get(ctx, remoteAgentID)
	if errors.Is(err, store.ErrRemoteAgentNotDeclared) {
		h.deny(ctx, w, span, rec, http.StatusForbidden,
			"remote agent "+remoteAgentID+" is not declared in the registry: a delegation destination must be declared, with an explicit risk, before anything can delegate to it")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	span.SetAttributes(attribute.String("aeon.a2a.remote_risk", remote.Risk))

	decision := h.Policy.IsAllowedToDelegate(rec.AgentManifestRef, remoteAgentID)
	if !decision.Allowed {
		h.deny(ctx, w, span, rec, http.StatusForbidden, "delegation "+denialReason(decision))
		return
	}

	// Width is checked only for a method that can START work. A poll for a result is traffic to the same
	// destination and cannot spawn anything, so counting it would make a caller that checks its tasks
	// look like one that launched more of them.
	var open *store.Delegation
	if rpc.StartsWork() {
		open, err = h.Delegations.Open(ctx, store.OpenOptions{
			Delegation: rec, MaxInFlight: h.MaxInFlightPerRun, StaleAfter: h.InFlightStaleAfter,
		})
		if inFlight, max, isFanOut := store.FanOutExceeded(err); isFanOut {
			span.SetStatus(codes.Error, "fan-out limit reached")
			h.deny(ctx, w, span, rec, http.StatusTooManyRequests,
				"run "+rec.RunID+" already has "+strconv.Itoa(inFlight)+" delegation(s) in flight, limit is "+strconv.Itoa(max))
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	upstream, err := h.forward(ctx, remote, rec, rpc.Method, body)
	if err != nil {
		if open != nil {
			// Closed with no state: the call is over from the proxy's side, and leaving the row open would
			// spend this run's width budget on a delegation nobody is waiting for.
			if cerr := h.Delegations.Close(ctx, open.ID, "", "", nil); cerr != nil {
				log.Printf("aeon-toolgw: closing delegation %d after a transport failure: %v", open.ID, cerr)
			}
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		writeError(w, http.StatusBadGateway, err)
		return
	}

	relayed, err := a2a.RelayResponse(upstream.body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	class := relayed.Classification

	terminal := class.Terminal
	switch {
	case open != nil && terminal:
		// The remote finished inside this one call. Row closed: the run's width is freed now.
		if err := h.Delegations.Close(ctx, open.ID, relayed.TaskID, string(class.State), &terminal); err != nil {
			log.Printf("aeon-toolgw: closing delegation %d: %v", open.ID, err)
		}
	case open != nil:
		// Observed, NOT closed. The remote task is still running — including when it is waiting for a
		// person over there (constraint (b)) — so this delegation is still outstanding and still counts
		// against the run's width. Closing it here would free width for something that has not finished,
		// which is the bookkeeping half of the very mistake constraint (b) forbids.
		if err := h.Delegations.Observe(ctx, open.ID, relayed.TaskID, string(class.State), &terminal); err != nil {
			log.Printf("aeon-toolgw: observing delegation %d: %v", open.ID, err)
		}
	case terminal && relayed.TaskID != "":
		// A poll (tasks/get) that found the task finished. This is the NORMAL way an asynchronous
		// delegation ends, and it is a different HTTP request from the one that opened the row — so the
		// row is closed by task id. Without this path a run's width would stay spent by tasks that
		// completed and were only ever observed through a poll.
		if n, err := h.Delegations.CloseByTaskID(ctx, rec.RunID, relayed.TaskID, string(class.State), &terminal); err != nil {
			log.Printf("aeon-toolgw: closing delegation for task %q: %v", relayed.TaskID, err)
		} else if n > 0 {
			span.SetAttributes(attribute.Int64("aeon.a2a.delegations_closed_by_poll", n))
		}
	}

	span.SetAttributes(
		attribute.String("aeon.a2a.task_state", string(class.State)),
		attribute.Bool("aeon.a2a.task_terminal", class.Terminal),
		attribute.Bool("aeon.a2a.task_state_recognised", class.Recognised),
		attribute.Bool("aeon.a2a.task_state_rewritten", relayed.StateRewritten),
	)
	span.SetStatus(codes.Ok, "")

	// The verbatim state travels in a header even when the body's was rewritten, so a substitution is
	// never invisible — an invisible rewrite in a proxy is how two sides come to disagree about what was
	// said.
	w.Header().Set(a2a.HeaderTaskState, string(class.State))
	w.Header().Set(a2a.HeaderTaskTerminal, strconv.FormatBool(class.Terminal))
	if class.PausedForHuman {
		w.Header().Set(a2a.HeaderPausedForHuman, "true")
	}
	if relayed.StateRewritten {
		w.Header().Set("X-Aeon-Remote-Task-State-Rewritten", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(upstream.status)
	if _, err := w.Write(relayed.Rewritten); err != nil {
		log.Printf("aeon-toolgw: relaying an A2A response: %v", err)
	}
}

type upstreamResponse struct {
	status int
	body   []byte
}

// forward sends the body to the declared destination with the credential injected here.
//
// The request is rebuilt rather than mutated, which is what guarantees the strip list is complete: only
// headers copied explicitly survive, so a header nobody thought about is dropped rather than forwarded.
// A deny-list on a mutated request would forward every header added to A2A after today.
func (h *A2AEgressHandlers) forward(
	ctx context.Context, remote *store.RemoteAgentRecord, rec store.Delegation, method string, body []byte,
) (upstreamResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, remote.URL, bytes.NewReader(body))
	if err != nil {
		return upstreamResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(a2a.HeaderDelegatingAgent, rec.AgentManifestRef)
	req.Header.Set(a2a.HeaderRemoteAgentID, remote.AgentID)
	if rec.RunID != "" {
		req.Header.Set(a2a.HeaderRunID, rec.RunID)
	}
	// Identity per hop: the depth the remote sees is one deeper than the one we were told. If the caller
	// declared none we send 1, because the call we are making is itself a hop — sending nothing would
	// make a chain look like it started at whatever point first bothered to count.
	next := 1
	if rec.HopDepth != nil {
		next = *rec.HopDepth + 1
	}
	req.Header.Set(a2a.HeaderHopDepth, strconv.Itoa(next))

	if remote.CredentialSecretName != "" {
		// Issued against the DELEGATING AGENT as owner, so A5's kill switch reaches it. Resolved and
		// revoked inside this request: the value exists in this process for the duration of one call and is
		// never handed to, or reachable by, the governed process that asked for the delegation.
		ref, _, err := h.Broker.IssueForOwner(remote.CredentialSecretName, delegationCredentialTTL, rec.AgentManifestRef)
		if err != nil {
			return upstreamResponse{}, err
		}
		defer h.Broker.Revoke(ref)
		value, err := h.Broker.Resolve(ref)
		if err != nil {
			return upstreamResponse{}, err
		}
		req.Header.Set("Authorization", "Bearer "+value)
	}

	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return upstreamResponse{}, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return upstreamResponse{}, err
	}
	return upstreamResponse{status: resp.StatusCode, body: out}, nil
}

// deny refuses a delegation and records that it was attempted.
//
// Recorded rather than only refused: "an agent tried to delegate somewhere it may not" is the event worth
// keeping, and a refusal that leaves no trace is indistinguishable from an agent that never tried.
func (h *A2AEgressHandlers) deny(
	ctx context.Context, w http.ResponseWriter, span trace.Span,
	rec store.Delegation, status int, reason string,
) {
	span.SetStatus(codes.Error, reason)
	if h.Delegations != nil {
		if err := h.Delegations.RecordDenial(ctx, rec, reason); err != nil {
			log.Printf("aeon-toolgw: recording a denied delegation to %q: %v", rec.RemoteAgentID, err)
		}
	}
	writeJSON(w, status, map[string]any{
		"allowed":         false,
		"reason":          reason,
		"remote_agent_id": rec.RemoteAgentID,
	})
}

// hopDepth reads the declared depth. A missing or unparseable value is NIL, not zero: unknown depth and
// depth zero are different facts, and recording the first as the second would invent the top of a chain.
func hopDepth(raw string) *int {
	if raw == "" {
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return nil
	}
	return &n
}
