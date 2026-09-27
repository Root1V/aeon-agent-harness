package a2a

import (
	"encoding/json"
	"fmt"
	"strings"

	sdka2a "github.com/a2aproject/a2a-go/a2a"
)

// Egress headers a governed process sends INBOUND to the proxy.
//
// Headers rather than body fields because the body is a JSON-RPC envelope that belongs to A2A and is
// forwarded VERBATIM. A proxy that rewrote the body to carry Aeon's attribution would stop being
// transparent, and transparency is the point: the framework points its A2A client at this endpoint and
// changes no code. That is the difference between this and the two designs it replaced — an advisory
// authorize-API the loop can skip, and an A2A client of our own the loop would have to adopt.
const (
	HeaderAgentManifestRef = "X-Aeon-Agent-Manifest-Ref"
	HeaderRunID            = "X-Aeon-Run-Id"
	HeaderStepID           = "X-Aeon-Step-Id"
	// HeaderHopDepth is how many delegations deep the CALLER believes it is. Advisory and Synaptum's to
	// enforce — depth is visible to whoever knows the tree, which is them. We record it so a chain can
	// be reconstructed afterwards, and we do not decide on it.
	HeaderHopDepth = "X-Aeon-Hop-Depth"
)

// Egress headers the proxy sets on the way OUT and on the way back.
const (
	// HeaderDelegatingAgent tells the remote which governed agent is calling. Identity per hop: the
	// remote learns who is delegating rather than seeing traffic from an anonymous gateway.
	HeaderDelegatingAgent = "X-Aeon-Delegating-Agent"
	// HeaderRemoteAgentID names the declared destination, so a remote fronting several agents can tell
	// which declaration this call was authorized against.
	HeaderRemoteAgentID = "X-Aeon-Remote-Agent-Id"
	// HeaderTaskState and the two below are set on the RESPONSE, for whoever is watching the proxy. The
	// caller's own SDK reads the JSON-RPC body; these are for humans and for tests, and they carry the
	// verbatim state even when the body's has been rewritten.
	HeaderTaskState      = "X-Aeon-Remote-Task-State"
	HeaderTaskTerminal   = "X-Aeon-Remote-Task-Terminal"
	HeaderPausedForHuman = "X-Aeon-Remote-Paused-For-Human"
)

// hopHeadersStrippedInbound are headers a governed process must not be able to set.
//
// Authorization is the important one: the credential for the destination is injected HERE, from the
// Secret Broker, so a value the caller supplied is discarded rather than forwarded. If it were merged
// instead, a governed process could reach the destination with a credential of its own and the whole
// arrangement would be decoration.
var hopHeadersStrippedInbound = []string{
	"Authorization", "Proxy-Authorization", "Cookie",
	HeaderDelegatingAgent, HeaderRemoteAgentID,
}

// RPCRequest is the part of a JSON-RPC 2.0 request the proxy needs to reason about.
//
// Deliberately partial: Params stays raw and is never re-encoded, so the body the remote receives is
// byte-for-byte what the framework sent. Re-marshalling would quietly normalise numbers and key order —
// the same class of divergence the step-identity corpus exists to pin down, and there is no reason to
// introduce it in a proxy that has no need to understand the payload.
type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// StartsWork reports whether this method can cause the remote to act.
//
// The distinction earns its keep in the ledger and in the width limit: a poll for a result is traffic to
// the same destination but cannot start anything, so counting it against fan-out would make a caller
// that checks its tasks look like one that spawned more of them.
func (r RPCRequest) StartsWork() bool {
	switch r.Method {
	case "message/send", "message/stream":
		return true
	default:
		return false
	}
}

// ParseRPCRequest reads the JSON-RPC envelope.
func ParseRPCRequest(body []byte) (RPCRequest, error) {
	var req RPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return RPCRequest{}, fmt.Errorf("a2a: egress request is not JSON-RPC: %w", err)
	}
	if req.Method == "" {
		return RPCRequest{}, fmt.Errorf("a2a: egress request names no method")
	}
	return req, nil
}

// RemoteResult is what the proxy learned from the remote's response.
type RemoteResult struct {
	TaskID         string
	Classification StateClassification
	// Rewritten is the response body to relay, which differs from the remote's only when an
	// unrecognised state had to be reported as `working` — see RelayResponse.
	Rewritten []byte
	// StateRewritten records that it happened, so the ledger and the operator can tell a pass-through
	// from a substitution. Without this the rewrite would be invisible, and an invisible rewrite in a
	// proxy is how two sides come to disagree about what was said.
	StateRewritten bool
}

// RelayResponse inspects the remote's JSON-RPC response and produces the body to hand back.
//
// THIS IS WHERE CONSTRAINT (a) IS ACTUALLY ENFORCED, and the distinction matters: classifying an unknown
// state in our own ledger changes nothing for the caller, whose SDK is the thing that would mishandle it.
// So a state this build does not recognise is REWRITTEN to `working` in the body that goes upward, while
// the verbatim state is kept in the classification, in the response headers and in the ledger. Nothing is
// lost; it is just not placed where a decision gets made.
//
// A response carrying a Message rather than a Task is terminal: the remote answered inline and there is
// nothing left to poll. That is not the same as an absent state on a Task, which is unrecognised and
// therefore still open.
func RelayResponse(body []byte) (RemoteResult, error) {
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		// Relayed unchanged. A body we cannot parse is the remote's business, and inventing an error
		// response here would hide whatever the remote actually said from the only party that can debug it.
		return RemoteResult{Rewritten: body, Classification: StateClassification{}}, nil
	}

	result, ok := envelope["result"].(map[string]any)
	if !ok {
		// A JSON-RPC error, or something else entirely. Passed through: an error from the remote is a real
		// answer and must reach the caller as it was sent.
		return RemoteResult{Rewritten: body}, nil
	}

	taskID, _ := result["id"].(string)
	status, hasStatus := result["status"].(map[string]any)
	if !hasStatus {
		// No status at all. A Message reply (kind == "message") has finished; anything else with no status
		// is not something we can call finished.
		kind, _ := result["kind"].(string)
		terminal := kind == "message"
		return RemoteResult{
			TaskID:    taskID,
			Rewritten: body,
			Classification: StateClassification{
				Terminal: terminal, Recognised: kind == "message",
			},
		}, nil
	}

	rawState, _ := status["state"].(string)
	class := ClassifyState(sdka2a.TaskState(rawState))
	out := RemoteResult{TaskID: taskID, Classification: class, Rewritten: body}

	if effective := EffectiveState(class); effective != class.State {
		status["state"] = string(effective)
		rewritten, err := json.Marshal(envelope)
		if err != nil {
			// Could not rewrite, so relay the original and say the state was NOT rewritten. Reporting a
			// rewrite that did not happen would be worse than the unrewritten body: a reader would believe
			// the caller had been protected from a state it is in fact about to see.
			return out, nil
		}
		out.Rewritten = rewritten
		out.StateRewritten = true
	}
	return out, nil
}

// StrippedInboundHeaders reports which caller-supplied headers the proxy will discard.
//
// Exposed so a test can assert on it rather than restating the list, which is how such a list and its
// test drift apart.
func StrippedInboundHeaders() []string {
	out := make([]string, len(hopHeadersStrippedInbound))
	copy(out, hopHeadersStrippedInbound)
	return out
}

// IsStrippedInbound reports whether a header name is discarded on the way out (case-insensitive).
func IsStrippedInbound(name string) bool {
	for _, h := range hopHeadersStrippedInbound {
		if strings.EqualFold(h, name) {
			return true
		}
	}
	return false
}
