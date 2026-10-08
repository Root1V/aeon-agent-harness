package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// A policy set: one Cedar engine per tenant (VRT-AEON-005 T-5).
//
// WHY A BUNDLE PER TENANT AND NOT THE TENANT INSIDE THE POLICIES. Veritium offered both shapes —
// "ya sea con un bundle por tenant o con el tenant en principal/recurso y guardas obligatorias" —
// and the second is the one this project has already been bitten by TWICE. A2A-002 measured that a
// separate Cedar resource type was necessary and NOT sufficient, because the bundle's permits list
// NAMES and said nothing about kind, so a remote agent called search.web inherited the tool permit.
// RUN-006 hit the same shape again with ExternalActivity. A guard that MUST be present is a guard
// somebody removes while tidying a `when` clause, and everything stays green.
//
// A bundle per tenant changes what a mistake does. There is no clause to forget: a tenant either has
// a bundle or it does not, and not having one is a DENIAL rather than a policy that quietly applies
// to everybody. It also gives each team what they asked for — writing their own policies without
// editing a file somebody else owns.
//
// WHAT IT COSTS, said rather than discovered: a policy that should apply to every tenant has to be
// written in every bundle. That is the honest trade. A "shared" bundle merged under each tenant's
// would be convenient and would reintroduce exactly the thing above — a rule applying where nobody
// wrote it.
type Set struct {
	byTenant map[string]*Engine
}

// tenantFileName fixes what a file in the policy directory may be called. The name IS the tenant, so
// it has to match what auth validates a tenant to be; anything else in that directory is refused
// rather than skipped, for the reason migrations.go gives about stray files: a bundle somebody
// believes is loaded and is not is worse than one that is missing.
var tenantFileName = regexp.MustCompile(`^([a-z][a-z0-9-]{0,62})\.ya?ml$`)

// LoadSetFromDir builds one engine per `<tenant>.yaml` in dir.
func LoadSetFromDir(dir string) (*Set, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("policy: reading the policy directory %s: %w", dir, err)
	}
	set := &Set{byTenant: map[string]*Engine{}}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		m := tenantFileName.FindStringSubmatch(entry.Name())
		if m == nil {
			return nil, fmt.Errorf(
				"policy: %s is not named <tenant>.yaml. The file name IS the tenant, so a file with "+
					"another name is a bundle nobody can be served — refused rather than skipped",
				filepath.Join(dir, entry.Name()))
		}
		engine, err := loadEngineFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		set.byTenant[m[1]] = engine
	}
	if len(set.byTenant) == 0 {
		return nil, fmt.Errorf("policy: %s holds no <tenant>.yaml bundle, so this gateway could authorize nothing", dir)
	}
	return set, nil
}

// LoadSetForSingleTenant is the one-bundle deployment, with the tenant named explicitly.
//
// It exists so an existing deployment keeps working WITHOUT the old implicit meaning. Before this,
// one bundle served whoever called; now that same file serves exactly one named tenant, and a caller
// from any other is denied. Same file, same policies, a statement that is now true.
func LoadSetForSingleTenant(tenant, path string) (*Set, error) {
	engine, err := loadEngineFile(path)
	if err != nil {
		return nil, err
	}
	return &Set{byTenant: map[string]*Engine{tenant: engine}}, nil
}

func loadEngineFile(path string) (*Engine, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("policy: reading policy bundle %s: %w", path, err)
	}
	var doc PolicyBundleDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("policy: parsing policy bundle %s: %w", path, err)
	}
	engine, err := LoadEngine(doc)
	if err != nil {
		return nil, fmt.Errorf("policy: loading %s: %w", path, err)
	}
	return engine, nil
}

// SingleTenantSet wraps one already-loaded engine as the set for one named tenant. For a caller that
// built its engine some other way — a test with an inline bundle, or a deployment that assembles the
// document itself — without making the tenant optional.
func SingleTenantSet(tenant string, e *Engine) *Set {
	return &Set{byTenant: map[string]*Engine{tenant: e}}
}

// EngineFor returns the tenant's engine, or false when that tenant has no bundle.
//
// FALSE IS A DENIAL AND NOT A FALLBACK. Returning some default engine would make an unconfigured
// tenant inherit somebody else's permits, which is the whole failure this shape exists to remove —
// and it would look like a working deployment. Every caller of this treats false as "refuse", and
// there is a test that fixes it.
func (s *Set) EngineFor(tenant string) (*Engine, bool) {
	if s == nil || tenant == "" {
		return nil, false
	}
	e, ok := s.byTenant[tenant]
	return e, ok
}

// Tenants lists the tenants this set can authorize, for the startup log. Sorted, so the line is
// stable and two deployments can be compared by eye.
func (s *Set) Tenants() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.byTenant))
	for t := range s.byTenant {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// String is the startup line: which tenants, never the policies.
func (s *Set) String() string {
	return strings.Join(s.Tenants(), ", ")
}
