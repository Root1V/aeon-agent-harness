package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

// CallersPathEnv is where every binary reads its caller bundle from.
const CallersPathEnv = "AEON_CALLERS_PATH"

// CallersDirEnv is the per-tenant alternative: one `<tenant>.yaml` per tenant.
//
// WHY IT EXISTS (VRT-AEON-005, asked by Veritium on 2026-10-08 when they closed that entry and
// asked how to hand us their shared-deployment configuration). Their policy bundle was a clean
// pull request — one file, named after their tenant, touching nothing else. Their CALLER entry was
// not: callers loaded from one file, so adding a consumer meant editing a document holding every
// other tenant's callers, token hashes included. One team's PR touching another team's credentials
// is not a review anybody can do well.
//
// It is the same shape GOV-001d gave the policy side, for the same reason, and callers are the
// other half of the same thing: policy says what a tenant may do, this says who the tenant is.
const CallersDirEnv = "AEON_CALLERS_DIR"

// callerFileName fixes what a file in the callers directory may be called. Same expression
// policy.tenantFileName uses, and the name IS the tenant — see LoadDir for why that matters more
// here than it does for a policy bundle.
var callerFileName = regexp.MustCompile(`^([a-z][a-z0-9-]{0,62})\.ya?ml$`)

// MustLoadFromEnv loads the caller bundle(s) named by AEON_CALLERS_PATH or AEON_CALLERS_DIR, or
// exits. Exactly one of the two: see the refusal below for why both set is not a precedence
// question.
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
	path, dir := os.Getenv(CallersPathEnv), os.Getenv(CallersDirEnv)

	// BOTH SET IS REFUSED rather than one winning, exactly as the policy pair already is. Whichever
	// lost would be a file an operator believes is loaded and is not — the same failure the stray-file
	// rule below exists for, one level up, and the kind that only shows up as a caller mysteriously
	// not authenticating.
	if path != "" && dir != "" {
		fatal("%s: %s and %s are both set. Pick one: a single bundle, or one <tenant>.yaml per tenant. "+
			"Honouring one of them would leave the other looking configured", serviceName, CallersPathEnv, CallersDirEnv)
	}
	if path == "" && dir == "" {
		fatal("%s: %s or %s is required (SEC-005). Either points at CallerBundle(s): see "+
			"examples/deep-research/callers.yaml", serviceName, CallersPathEnv, CallersDirEnv)
	}

	load, source := LoadFile, path
	if dir != "" {
		load, source = LoadDir, dir
	}
	a, err := load(source)
	if err != nil {
		fatal("%s: loading the caller bundle(s) %s: %v", serviceName, source, err)
	}
	return a
}

// LoadDir reads one `<tenant>.yaml` per tenant from dir and validates them AS ONE BUNDLE.
//
// THE FILE NAME IS THE TENANT, and every caller inside must declare that same tenant. This is a
// stronger rule than the policy side needs and it is the whole point: a bundle governs one tenant
// and names none inside, but a caller DECLARES its tenant, so without this `veritium.yaml` could
// declare a caller in tenant `otro` and nothing would stop it. The file name is what a reviewer can
// check at a glance, and binding the tenant to it is what makes "one file per team" a boundary
// rather than a filing convention.
//
// MERGED AND THEN VALIDATED, not validated file by file. Every rule Load already enforces —
// duplicate ids, duplicate token hashes, a required tenant, mayActForTenants being service-only —
// has to hold ACROSS the directory, and the one that matters most is the token hash: two teams do
// not see each other's file, so a shared token is the collision that will actually happen. Checked
// per file it would pass twice and the audit trail's identity would be decided by read order.
//
// A STRAY FILE IS REFUSED rather than skipped, for the reason migrations.go gives: a file somebody
// believes is loaded and is not is worse than one that is missing. That includes a README.
func LoadDir(dir string) (*Authenticator, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("auth: reading the callers directory %s: %w", dir, err)
	}

	var merged CallerBundleDoc
	merged.Kind = "CallerBundle"
	// Which file each caller came from, so a cross-file collision can name both files instead of
	// reporting a duplicate the operator then has to go and find.
	fileOf := map[string]string{}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	// Sorted, so the error an operator sees does not depend on the filesystem's read order.
	sort.Strings(names)

	for _, name := range names {
		m := callerFileName.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf(
				"auth: %s is not named <tenant>.yaml. The file name IS the tenant, so a file with another "+
					"name declares callers for nobody — refused rather than skipped, because a file an "+
					"operator believes is loaded and is not is worse than one that is missing",
				filepath.Join(dir, name))
		}
		tenant := m[1]

		raw, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			return nil, fmt.Errorf("auth: reading caller bundle %s: %w", filepath.Join(dir, name), readErr)
		}
		var doc CallerBundleDoc
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("auth: parsing caller bundle %s: %w", filepath.Join(dir, name), err)
		}
		if doc.Kind != "" && doc.Kind != "CallerBundle" {
			return nil, fmt.Errorf("auth: %s declares kind %q, want CallerBundle", filepath.Join(dir, name), doc.Kind)
		}
		if len(doc.Callers) == 0 {
			// Not tolerated as "a tenant with no callers yet": the file exists, so somebody wrote it
			// expecting it to do something, and an empty one does nothing while looking configured.
			return nil, fmt.Errorf("auth: %s declares no callers", filepath.Join(dir, name))
		}

		for _, c := range doc.Callers {
			if c.Tenant != tenant {
				return nil, fmt.Errorf(
					"auth: %s declares caller %q with tenant %q. The file name is the tenant, so this file "+
						"may only declare callers for %q — otherwise one team's file could add a caller in "+
						"another team's tenant, which is the thing a file per tenant exists to prevent",
					filepath.Join(dir, name), c.ID, c.Tenant, tenant)
			}
			if previous, dup := fileOf[c.ID]; dup {
				return nil, fmt.Errorf(
					"auth: caller id %q is declared in both %s and %s", c.ID, previous, filepath.Join(dir, name))
			}
			fileOf[c.ID] = filepath.Join(dir, name)
			merged.Callers = append(merged.Callers, c)
		}
	}

	// ErrNoCallers from Load covers the empty directory, and it has to stay fatal: a service with no
	// callers starts, looks healthy and authenticates nobody, which MustLoadFromEnv's own comment
	// calls worse than not starting.
	a, err := Load(merged)
	if err != nil {
		return nil, fmt.Errorf("auth: validating the callers in %s as one bundle: %w", dir, err)
	}
	return a, nil
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
