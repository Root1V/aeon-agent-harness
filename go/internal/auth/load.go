package auth

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// CallersPathEnv is where every binary reads its caller bundle from.
const CallersPathEnv = "AEON_CALLERS_PATH"

// MustLoadFromEnv loads the caller bundle named by AEON_CALLERS_PATH, or exits.
//
// FATAL AND NOT A WARNING, which is the decision this whole feature rests on. A service that starts
// without a caller bundle has two possible behaviours and both are worse than not starting: serve
// everything unauthenticated (what SEC-005 exists to end) or serve nothing while looking healthy. The
// repo already takes this position for the policy bundle — `aeon-toolgw` log.Fatals without
// AEON_POLICY_BUNDLE_PATH — and a gateway that enforces policy against an unverified identity is not
// meaningfully better off than one with no policy at all.
//
// The message names the variable and the file shape, because the operator reading it is the one who
// has not configured this yet.
func MustLoadFromEnv(serviceName string) *Authenticator {
	path := os.Getenv(CallersPathEnv)
	if path == "" {
		fatal("%s: %s is required (SEC-005). It points at a CallerBundle: see examples/deep-research/callers.yaml", serviceName, CallersPathEnv)
	}
	a, err := LoadFile(path)
	if err != nil {
		fatal("%s: loading the caller bundle %s: %v", serviceName, path, err)
	}
	return a
}

// LoadFile reads and validates a caller bundle from disk.
func LoadFile(path string) (*Authenticator, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth: reading caller bundle: %w", err)
	}
	var doc CallerBundleDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("auth: parsing caller bundle %s: %w", path, err)
	}
	return Load(doc)
}
