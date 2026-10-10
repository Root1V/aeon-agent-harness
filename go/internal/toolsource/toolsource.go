// Package toolsource is TOOL-010: an external MCP server registered as a source of tools, so the
// governance of those tools — who may call which, the audit, the cost — lives in one place instead
// of being duplicated in every platform that owns a tool.
//
// WHAT THIS IS NOT. INT-003 is the opposite direction, already done: Aeon as an MCP *server*
// exposing its own catalogue. TOOL-002 built the MCP *client* and left it with no production
// caller; the package doc there said "the Tool Gateway, eventually". This is the eventually.
//
// THE OPERATOR APPROVES EVERY TOOL, ONE BY ONE, AND THAT IS THE DESIGN. `tools/list` on a source
// reports a name, a description and an input schema — never a side effect, never a risk, never
// which arguments form an idempotency key. The registry refuses anything beyond READ_ONLY that does
// not declare `idempotency_key_fields` (ADR-0001), and a discovered tool brings none of it. A source
// that classified itself would make that rule optional: declare everything READ_ONLY and it stops
// applying. So the bundle below is an attestation, not a cache: a tool Aeon serves is one a person
// read and classified, and a tool the source offers that nobody listed is not served.
//
// Agreed with Veritium in VRT-AEON-006: they supply a suggested classification under MCP's `_meta`
// and we treat it as INPUT to that decision, never as the decision.
package toolsource

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Bundle is the operator's declaration of every external tool source and the tools approved from
// each. One file, like the policy bundle and the caller bundle it sits beside.
type Bundle struct {
	Kind    string   `yaml:"kind"`
	Sources []Source `yaml:"sources"`
}

// Source is one external MCP server.
type Source struct {
	ID       string `yaml:"id"`
	Endpoint string `yaml:"endpoint"`
	// Prefix namespaces every tool from this source. REQUIRED, because the catalogue is keyed by
	// tool name (`server.AddTool(name)`), so two sources that both offer `search.web` would collide
	// in silence with the last one of a refresh winning. Veritium proposed that the registered
	// source fix it rather than the source itself, which is also the only way it can be trusted.
	Prefix string `yaml:"prefix"`
	// Refresh is how often the source is re-listed. Zero means DefaultRefresh.
	Refresh time.Duration  `yaml:"refresh"`
	Auth    *Auth          `yaml:"auth"`
	Tools   []ApprovedTool `yaml:"tools"`
}

// Auth is OAuth2 client-credentials against the source's own token endpoint.
//
// THE CREDENTIAL IS NAMED, NEVER WRITTEN. Both fields hold the NAME of an environment variable, so
// this file can be committed and reviewed: a bundle that carried the secret itself would be read by
// everyone who can read the repository, and the one thing this session keeps proving is that a
// secret in a tracked file is a secret that has already leaked.
type Auth struct {
	TokenURL        string   `yaml:"token_url"`
	ClientIDEnv     string   `yaml:"client_id_env"`
	ClientSecretEnv string   `yaml:"client_secret_env"`
	Scopes          []string `yaml:"scopes"`
}

// ApprovedTool is one tool a person read, classified and pinned.
type ApprovedTool struct {
	// Name is the name ON THE SOURCE, without the prefix: what `tools/list` reports there.
	Name string `yaml:"name"`
	// SideEffect and Risk are the registry's own required classification (TOOL-001/ADR-0001). There
	// is no default and no inference: a wrong guess here is a tool with effects treated as a read.
	SideEffect string `yaml:"side_effect"`
	Risk       string `yaml:"risk"`
	// IdempotencyKeyFields is required when SideEffect is not READ_ONLY, which is the registry's
	// rule restated at the point where a federated tool enters — enforcing it only at the registry
	// would let a misconfigured source fail at call time instead of at startup.
	IdempotencyKeyFields []string `yaml:"idempotency_key_fields"`
	// DescriptorSHA256 pins the descriptor this approval was given for: the RFC 8785 canonical form
	// of {name, description, input_schema}, hashed.
	//
	// THIS IS THE HALF THAT CANNOT BE ADDED LATER. The classification above describes a specific
	// tool with a specific input schema, and the source can change that schema — or its meaning at
	// the same schema — between two refreshes, with no redeploy and no review on our side. Without a
	// pin, "a person approved this" decays into "a person approved something that used to be here".
	// Omitting it is refused rather than defaulted: a pin that defaults to "whatever arrives" is not
	// a pin.
	DescriptorSHA256 string `yaml:"descriptor_sha256"`
}

// DefaultRefresh matches the catalogue's own poll interval in spirit: often enough that a tool
// added at the source appears without a restart, rarely enough that a handful of sources do not
// become a traffic source of their own.
const DefaultRefresh = 60 * time.Second

var (
	sourceIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	prefixPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}\.$`)
)

// Load reads and validates a bundle from path.
func Load(path string) (*Bundle, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("toolsource: reading %s: %w", path, err)
	}
	var b Bundle
	if err := yaml.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("toolsource: parsing %s: %w", path, err)
	}
	if err := b.Validate(); err != nil {
		return nil, fmt.Errorf("toolsource: %s: %w", path, err)
	}
	return &b, nil
}

// Validate refuses a bundle that would serve something nobody approved, name something ambiguously,
// or send a credential in the clear.
//
// AN EMPTY BUNDLE IS AN ERROR, not an empty catalogue. The policy loader learned this the hard way:
// a bundle whose key was misspelled parsed into zero policies and denied everything, because YAML
// drops a key it does not know. A file that exists and declares nothing is a configuration mistake
// far more often than it is an intention.
func (b *Bundle) Validate() error {
	if b.Kind != "ToolSourceBundle" {
		return fmt.Errorf("kind is %q, want %q", b.Kind, "ToolSourceBundle")
	}
	if len(b.Sources) == 0 {
		return fmt.Errorf("no sources declared: a bundle that exists and declares nothing is a mistake more often than an intention")
	}

	seenID := map[string]string{}
	seenPrefix := map[string]string{}
	for i := range b.Sources {
		s := &b.Sources[i]
		if !sourceIDPattern.MatchString(s.ID) {
			return fmt.Errorf("source %d: id %q must match %s", i, s.ID, sourceIDPattern)
		}
		if prev, dup := seenID[s.ID]; dup {
			return fmt.Errorf("source id %q declared twice (%s)", s.ID, prev)
		}
		seenID[s.ID] = "earlier in this bundle"

		if !prefixPattern.MatchString(s.Prefix) {
			return fmt.Errorf("source %q: prefix %q must match %s — the catalogue is keyed by tool name, "+
				"so two sources without distinct prefixes collide in silence", s.ID, s.Prefix, prefixPattern)
		}
		if prev, dup := seenPrefix[s.Prefix]; dup {
			return fmt.Errorf("source %q: prefix %q is already used by source %q — distinct prefixes are the "+
				"whole protection against a name collision", s.ID, s.Prefix, prev)
		}
		seenPrefix[s.Prefix] = s.ID

		if err := validateEndpoint(s.ID, s.Endpoint); err != nil {
			return err
		}
		if s.Auth != nil {
			if err := validateAuth(s.ID, s.Auth); err != nil {
				return err
			}
		}
		if len(s.Tools) == 0 {
			return fmt.Errorf("source %q approves no tools: it would be connected to and serve nothing", s.ID)
		}
		seenTool := map[string]bool{}
		for j := range s.Tools {
			t := &s.Tools[j]
			if t.Name == "" {
				return fmt.Errorf("source %q tool %d: name is required", s.ID, j)
			}
			if seenTool[t.Name] {
				return fmt.Errorf("source %q: tool %q approved twice", s.ID, t.Name)
			}
			seenTool[t.Name] = true
			if t.SideEffect == "" || t.Risk == "" {
				return fmt.Errorf("source %q tool %q: side_effect and risk are required and are never inferred — "+
					"`tools/list` does not report them, and guessing turns a tool with effects into a read",
					s.ID, t.Name)
			}
			if t.SideEffect != "READ_ONLY" && len(t.IdempotencyKeyFields) == 0 {
				return fmt.Errorf("source %q tool %q: side_effect %q needs idempotency_key_fields (ADR-0001), "+
					"checked here so a misconfiguration fails at startup rather than at the first call",
					s.ID, t.Name, t.SideEffect)
			}
			if len(t.DescriptorSHA256) != 64 {
				return fmt.Errorf("source %q tool %q: descriptor_sha256 must be the 64-character hex digest of the "+
					"descriptor this approval was given for. Start the gateway once without it and the log prints "+
					"the digest it observed, review that descriptor, then paste it here", s.ID, t.Name)
			}
		}
	}
	return nil
}

// validateEndpoint requires TLS unless the host is loopback.
//
// The exception is for a test server and a local dev source, which is where every acceptance test
// of this feature runs; everything else carries a bearer token over the wire, and the same rule the
// compose port check enforces applies here for the same reason — a plaintext endpoint works fine
// and is simply readable by anyone on the path.
func validateEndpoint(id, endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return fmt.Errorf("source %q: endpoint %q is not a URL", id, endpoint)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("source %q: endpoint %q is plaintext http to a non-loopback host — a bearer token "+
			"would go over the wire in the clear", id, endpoint)
	default:
		return fmt.Errorf("source %q: endpoint scheme %q is not http(s)", id, u.Scheme)
	}
}

func isLoopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateAuth(id string, a *Auth) error {
	if a.TokenURL == "" || a.ClientIDEnv == "" || a.ClientSecretEnv == "" {
		return fmt.Errorf("source %q: auth needs token_url, client_id_env and client_secret_env", id)
	}
	if u, err := url.Parse(a.TokenURL); err != nil || u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return fmt.Errorf("source %q: auth.token_url %q must be https (or loopback http)", id, a.TokenURL)
	}
	// A VALUE HERE IS A MISTAKE WORTH REFUSING, not a convenience to tolerate: these fields hold the
	// NAME of an environment variable, and the most likely way to get that wrong is to paste the
	// secret. Refusing anything that does not look like a variable name is what keeps the file
	// committable.
	for _, f := range []struct{ field, value string }{
		{"client_id_env", a.ClientIDEnv}, {"client_secret_env", a.ClientSecretEnv},
	} {
		if !regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`).MatchString(f.value) {
			return fmt.Errorf("source %q: auth.%s must be the NAME of an environment variable "+
				"(upper snake case), not a value — this file is meant to be committed", id, f.field)
		}
	}
	return nil
}

// RefreshInterval resolves the source's poll interval.
func (s *Source) RefreshInterval() time.Duration {
	if s.Refresh > 0 {
		return s.Refresh
	}
	return DefaultRefresh
}

// Approval finds the operator's approval for a tool name as the source reports it.
func (s *Source) Approval(sourceToolName string) (*ApprovedTool, bool) {
	for i := range s.Tools {
		if s.Tools[i].Name == sourceToolName {
			return &s.Tools[i], true
		}
	}
	return nil, false
}

// QualifiedName is the name Aeon serves this tool under.
func (s *Source) QualifiedName(sourceToolName string) string { return s.Prefix + sourceToolName }
