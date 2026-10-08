// Package auth is SEC-005: who is calling, verified, and what they are allowed to claim.
//
// WHAT WAS WRONG, and it is the gap between "governed" and "governed if you trust everyone on the
// network". Before this package:
//
//   - No Aeon HTTP surface authenticated anything. The whole repo's only `Authorization` header was
//     OUTBOUND (the A2A egress). Whoever reached :9404 could start, cancel, pause and APPROVE runs;
//     whoever reached :9403 could execute tools. All four services publish on 0.0.0.0 — the Argus
//     stack beside them deliberately binds 127.0.0.1.
//   - The Cedar principal came from the request body: `IsAllowed(body.AgentManifestRef, ...)`. The
//     policy engine was real, default-deny and well tested, and the identity it judged was whatever
//     the caller typed. A caller could name any agent and be judged as that agent.
//   - An approval had no approver. `Approve(ctx, workflowID, toolCallHash)` recorded
//     `approval_granted` and could not say who granted it, so the audit trail of an irreversible
//     action named the action and not the person.
//
// THE MODEL, and the distinction it keeps is the whole design. A CALLER is not an AGENT. The caller is
// the process or person holding a credential; the agent manifest is whose policy applies to the work.
// Collapsing them would have been simpler and wrong: the worker runs many agents, and a human
// approving a step is not an agent at all. So the body still names the agent, and a caller may only
// claim the agents its own entry lists. Cedar keeps judging agents; this decides who is allowed to
// speak for one.
//
// AND APPROVAL IS A SEPARATE PERMISSION FROM EXECUTION, which is the single most important property
// here. A worker that could approve could approve the irreversible action it is itself asking about,
// and the gate would be decoration. `mayApprove` is therefore never implied by `mayActAs`.
//
// NO PLAINTEXT TOKENS IN THE BUNDLE. The file carries `tokenSHA256` and the token lives wherever the
// operator keeps secrets. A config-as-code file is committed, diffed and pasted into issues, and a
// secret in one is a secret in all three places.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
)

// Kind is what sort of principal a caller is. Closed, because the set answers a question that must
// stay answerable: "was this approved by a person or by a process".
type Kind string

const (
	// KindService is a process: a worker, a gateway, another Aeon service.
	KindService Kind = "service"
	// KindHuman is a person holding a credential. Only a human may approve by default — see
	// CallerBundleDoc's validation.
	KindHuman Kind = "human"
	// KindExternal is a caller outside this deployment: an MCP client, another org's agent. Named
	// separately from service because "ours" and "someone else's" are different trust answers, and
	// INT-003 already needed the distinction for its shared Cedar principal.
	KindExternal Kind = "external"
)

// Caller is an authenticated principal.
type Caller struct {
	ID   string `yaml:"id" json:"id"`
	Kind Kind   `yaml:"kind" json:"kind"`
	// TokenSHA256 is the hex-encoded SHA-256 of the caller's bearer token. Never the token.
	TokenSHA256 string `yaml:"tokenSHA256" json:"-"`
	// MayActAs lists the agent_manifest_refs this caller may present to the Tool Gateway. No wildcard
	// is supported, on purpose: a `*` would be convenient, would spread by habit, and would restore
	// exactly the hole this package closes while looking configured.
	MayActAs []string `yaml:"mayActAs" json:"may_act_as,omitempty"`
	// MayApprove is whether this caller may decide a pending approval. Never implied by MayActAs.
	MayApprove bool `yaml:"mayApprove" json:"may_approve"`
	// Tenant is the isolation boundary this caller's reads and writes are confined to
	// (VRT-AEON-005 T-1). REQUIRED: a bundle with a caller that has none does not load.
	//
	// ASSIGNED BY THE OPERATOR OF THE DEPLOYMENT, NEVER DECLARED BY THE CLIENT IN A REQUEST, which
	// Veritium asked for in those words and which is the lesson SEC-005 already paid for: the Cedar
	// principal used to come off the wire, so a well-tested default-deny engine was judging whichever
	// identity the caller typed. A tenant taken from a request body or query string is the same
	// defect with a different field name, and it is the one the Memory Store had — every memory route
	// read `tenant_id` from the request, so the isolation SEC-004 built and tested was real and the
	// tenant was the caller's choice.
	Tenant string `yaml:"tenant" json:"tenant"`
}

// ActsAs reports whether this caller may present agentManifestRef as its own.
func (c Caller) ActsAs(agentManifestRef string) bool {
	for _, allowed := range c.MayActAs {
		if allowed == agentManifestRef {
			return true
		}
	}
	return false
}

// CallerBundleDoc is the config-as-code shape, mirroring policy.PolicyBundleDoc so an operator meets
// one convention and not two.
type CallerBundleDoc struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Callers    []Caller `yaml:"callers"`
}

// Authenticator resolves a bearer token to a Caller.
type Authenticator struct {
	// byTokenHash, not a slice scanned per request: an O(callers) comparison would still be constant
	// time per entry, but the map keeps the lookup independent of how many callers exist.
	byTokenHash map[string]Caller
}

var (
	// ErrNoCallers is returned for a bundle that authenticates nobody. Loading it would leave a
	// service running with authentication configured and no one able to pass it, which an operator
	// would then "fix" by removing the requirement.
	ErrNoCallers = errors.New("auth: the caller bundle declares no callers")
	// ErrUnauthenticated is what a request without a usable credential gets.
	ErrUnauthenticated = errors.New("auth: no valid bearer token")
)

// Load validates a bundle and builds an Authenticator.
//
// Every validation here refuses rather than repairs. A caller with no token hash, a duplicate id, a
// duplicate token or an unknown kind is a mistake in a security config, and the one thing worse than
// failing to start is starting with an entry that does not mean what it looks like.
// tenantPattern keeps a tenant to a plain lowercase identifier. Deliberately narrow: this value
// ends up in a SQL predicate and (from slice 4) in a Cedar principal, and a narrow alphabet means
// neither place needs quoting rules that could be got wrong once.
var tenantPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

func Load(doc CallerBundleDoc) (*Authenticator, error) {
	if doc.Kind != "" && doc.Kind != "CallerBundle" {
		return nil, fmt.Errorf("auth: expected kind CallerBundle, got %q", doc.Kind)
	}
	if len(doc.Callers) == 0 {
		return nil, ErrNoCallers
	}
	a := &Authenticator{byTokenHash: make(map[string]Caller, len(doc.Callers))}
	seenID := make(map[string]bool, len(doc.Callers))
	for i, c := range doc.Callers {
		if c.ID == "" {
			return nil, fmt.Errorf("auth: caller %d has no id", i)
		}
		if seenID[c.ID] {
			return nil, fmt.Errorf("auth: duplicate caller id %q", c.ID)
		}
		seenID[c.ID] = true
		switch c.Kind {
		case KindService, KindHuman, KindExternal:
		default:
			return nil, fmt.Errorf("auth: caller %q has kind %q, want one of service|human|external", c.ID, c.Kind)
		}
		// VRT-AEON-005 T-1: REQUIRED, and refused rather than defaulted. A default tenant would mean
		// a caller whose entry forgot one silently joins whichever tenant the default names, and the
		// deployment looks configured — the same shape as the `mayActAs` wildcard this package
		// refuses one field above. There is no "all tenants" value for the same reason.
		c.Tenant = strings.TrimSpace(c.Tenant)
		if c.Tenant == "" {
			return nil, fmt.Errorf(
				"auth: caller %q has no tenant. It is required and has no default: a caller that joined "+
					"a default tenant by omission would read and write another deployment's data while "+
					"looking configured", c.ID)
		}
		if !tenantPattern.MatchString(c.Tenant) {
			return nil, fmt.Errorf(
				"auth: caller %q has tenant %q, which must match %s — a plain identifier, so it can be "+
					"a safe part of a database predicate and of a Cedar principal without quoting rules",
				c.ID, c.Tenant, tenantPattern)
		}

		hash := strings.ToLower(strings.TrimSpace(c.TokenSHA256))
		if len(hash) != 64 {
			return nil, fmt.Errorf("auth: caller %q needs a 64-character hex tokenSHA256 (got %d characters)", c.ID, len(hash))
		}
		if _, err := hex.DecodeString(hash); err != nil {
			return nil, fmt.Errorf("auth: caller %q has a non-hex tokenSHA256: %w", c.ID, err)
		}
		if other, dup := a.byTokenHash[hash]; dup {
			// Two callers sharing a token is not two identities, it is one identity with two names —
			// and whichever the map returned would decide the audit trail.
			return nil, fmt.Errorf("auth: callers %q and %q share a token", other.ID, c.ID)
		}
		c.TokenSHA256 = hash
		a.byTokenHash[hash] = c
	}
	return a, nil
}

// Authenticate resolves a raw bearer token.
func (a *Authenticator) Authenticate(token string) (Caller, error) {
	if a == nil || token == "" {
		return Caller{}, ErrUnauthenticated
	}
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	candidate, ok := a.byTokenHash[hash]
	if !ok {
		return Caller{}, ErrUnauthenticated
	}
	// Constant-time even after a map hit. The map lookup already leaked nothing useful (it compares
	// hashes, not tokens), and this keeps the comparison honest if the storage ever changes to a scan.
	if subtle.ConstantTimeCompare([]byte(hash), []byte(candidate.TokenSHA256)) != 1 {
		return Caller{}, ErrUnauthenticated
	}
	return candidate, nil
}

type callerContextKey struct{}

// WithCaller puts an authenticated caller in a context. Exported for tests and for non-HTTP entry
// points; ordinary request handling goes through Require.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerContextKey{}, c)
}

// CallerFrom returns the authenticated caller, or false when there is none.
//
// FALSE IS NOT "ANONYMOUS IS FINE". Every handler that reads this must refuse when it gets false —
// absence means the request never passed Require, which is a wiring mistake, and treating it as a
// permissive default is how an unauthenticated route appears one handler at a time.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerContextKey{}).(Caller)
	return c, ok
}

// Require is the middleware every authenticated surface is wrapped in.
func Require(a *Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			caller, err := a.Authenticate(bearerToken(r))
			if err != nil {
				// WWW-Authenticate because a 401 without it tells a client it failed and not how to
				// succeed, and the reason text names the config so an operator reads one message
				// instead of grepping.
				w.Header().Set("WWW-Authenticate", `Bearer realm="aeon"`)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthenticated: send Authorization: Bearer <token> for a caller declared in the caller bundle (AEON_CALLERS_PATH)"}`))
				return
			}
			next.ServeHTTP(w, r.WithContext(WithCaller(r.Context(), caller)))
		})
	}
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// fatal is log.Fatalf, indirected so the package does not import log into every file and so a test
// could replace it if one ever needs to assert the message rather than the exit.
var fatal = func(format string, args ...any) {
	log.Fatalf(format, args...)
}

// HashToken is the hex SHA-256 an operator puts in a bundle. Exported so `aeon` and the tests derive
// it from this code instead of each shelling out to a different `sha256sum` incantation.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
