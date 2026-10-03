package secretref_test

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// composeDoc is the only part of the compose schema this test needs. `environment` is a mapping in
// this file; the list form (`- KEY=value`) would decode into nothing here, which is why the test
// asserts it found the services it expects rather than passing on an empty parse.
type composeDoc struct {
	Services map[string]struct {
		Environment map[string]*string `yaml:"environment"`
		EnvFile     any                `yaml:"env_file"`
	} `yaml:"services"`
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// go/internal/secretref/compose_blast_radius_test.go -> four levels up.
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}

func loadCompose(t *testing.T) composeDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "compose", "docker-compose.yml"))
	if err != nil {
		t.Fatalf("reading the reference stack: %v", err)
	}
	var doc composeDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the reference stack: %v", err)
	}
	for _, name := range []string{"controlplane", "modelgw", "toolgw", "worker", "runcontroller"} {
		if _, ok := doc.Services[name]; !ok {
			t.Fatalf("service %q not found — this test parsed something, but not the stack it is about", name)
		}
	}
	return doc
}

// TestNoServiceLoadsTheWholeEnvFile fixes the MECHANISM rather than today's outcome.
//
// Three services carried `env_file: ../../.env`, and the measured result was that every secret in
// that file reached every one of them: aeon-controlplane's environment held PROMETHEUS_CLIENT_SECRET
// and OPENAI_COMPATIBLE_API_KEY, neither of which appears anywhere in go/cmd/aeon-controlplane, and
// aeon-toolgw — the process that EXECUTES tools — held all three cloud provider keys for the same
// reason. Nothing was wrong with any single line; the blast radius was a property of the mechanism.
//
// So this test bans the mechanism. `env_file` with a per-service file is fine and would pass; what
// it refuses is a service pulling in the repository's whole credential file, which is how one added
// credential silently widens three processes.
func TestNoServiceLoadsTheWholeEnvFile(t *testing.T) {
	doc := loadCompose(t)
	for name, svc := range doc.Services {
		if svc.EnvFile == nil {
			continue
		}
		rendered, err := yaml.Marshal(svc.EnvFile)
		if err != nil {
			t.Fatalf("%s: re-rendering env_file: %v", name, err)
		}
		if strings.Contains(string(rendered), "../../.env") {
			t.Errorf("service %q loads the repository's whole .env.\n"+
				"Name the credentials it actually uses in its own `environment:` block instead. "+
				"The measurement that produced this rule: with env_file in place, aeon-controlplane "+
				"held PROMETHEUS_CLIENT_SECRET and OPENAI_COMPATIBLE_API_KEY with no code that reads "+
				"either, and aeon-toolgw held all three cloud provider keys.", name)
		}
	}
}

// TestEachSecretReachesOnlyTheServicesThatReadIt is the outcome half, and the expectations below are
// each a claim about the CODE: the services listed are the ones where a grep for that variable finds
// a non-test read. When this fails, the question is not "update the list" — it is whether the service
// that gained a credential has anything that uses it.
func TestEachSecretReachesOnlyTheServicesThatReadIt(t *testing.T) {
	doc := loadCompose(t)

	expected := map[string]struct {
		services []string
		because  string
	}{
		"ANTHROPIC_API_KEY": {[]string{"modelgw"},
			"go/cmd/aeon-modelgw registers the anthropic adapter; no other binary mentions the key"},
		"OPENAI_API_KEY": {[]string{"modelgw"},
			"same: the openai adapter lives in aeon-modelgw alone"},
		"GOOGLE_API_KEY": {[]string{"modelgw"},
			"same: the gemini adapter lives in aeon-modelgw alone"},
		"OPENAI_COMPATIBLE_API_KEY": {[]string{"modelgw"},
			"MDL-007's generic adapter is registered only by aeon-modelgw"},
		"PROMETHEUS_CLIENT_SECRET": {[]string{"modelgw", "toolgw"},
			"TWO services genuinely read it: aeon-modelgw for MDL-006 routing, aeon-toolgw for " +
				"TOOL-006's embedder (ragEmbedderFromEnv). Not aeon-controlplane, which held it before SEC-006"},
		"AEON_MEMORY_HMAC_KEY": {[]string{"controlplane"},
			"MEM-001's provenance_hmac is computed in aeon-controlplane's store and nowhere else"},
		"AEON_CALLER_TOKEN": {[]string{"worker"},
			"the worker is the only CALLER in the stack. The other three VERIFY tokens against the " +
				"caller bundle and never present one — and before SEC-006 the token reached exactly " +
				"those three and not the worker, so rotating it in .env rotated nothing"},
	}

	for secret, want := range expected {
		var got []string
		for name, svc := range doc.Services {
			if _, declared := svc.Environment[secret]; declared {
				got = append(got, name)
			}
		}
		sort.Strings(got)
		sort.Strings(want.services)
		if strings.Join(got, ",") != strings.Join(want.services, ",") {
			t.Errorf("%s is declared for %v, want %v.\nWhy: %s", secret, got, want.services, want.because)
		}
	}
}

// TestTheFileOverrideClearsEveryVariableItReplaces is the one property of secrets.override.yml that
// cannot be left to review. secretref REFUSES a service with both <NAME> and <NAME>_FILE set, so an
// override that adds the _FILE variable without clearing the plain one does not fall back to the
// environment — it stops the service from starting, after an `up` that reported success.
func TestTheFileOverrideClearsEveryVariableItReplaces(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "compose", "secrets.override.yml"))
	if err != nil {
		t.Fatalf("reading the secrets override: %v", err)
	}
	var doc composeDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the secrets override: %v", err)
	}
	if len(doc.Services) == 0 {
		t.Fatal("the override declares no services; this test would pass over nothing")
	}

	checked := 0
	for name, svc := range doc.Services {
		for key, value := range svc.Environment {
			base, isFileVar := strings.CutSuffix(key, "_FILE")
			if !isFileVar {
				continue
			}
			checked++
			if value == nil || *value == "" {
				t.Errorf("%s: %s is set but has no value", name, key)
			}
			cleared, present := svc.Environment[base]
			if !present {
				t.Errorf("%s: %s is set but %s is not cleared. The base stack supplies %s from .env, "+
					"and secretref refuses a service that has both — so this service would not start.",
					name, key, base, base)
				continue
			}
			if cleared != nil && *cleared != "" {
				t.Errorf("%s: %s is set to %q alongside %s; it must be the empty string, which is "+
					"what secretref reads as \"not configured\"", name, base, *cleared, key)
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no _FILE variables in the override: it is supposed to be the file path")
	}
}
