package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const bundleA = `
apiVersion: harness.ai/v1
kind: PolicyBundle
policies:
  - id: allow-a-tools
    effect: permit
    cedarSource: |
      permit(principal == Agent::"shared-name@1.0.0", action, resource)
      when { resource is Tool && resource.name == "search.web" };
`

const bundleB = `
apiVersion: harness.ai/v1
kind: PolicyBundle
policies:
  - id: allow-b-tools
    effect: permit
    cedarSource: |
      permit(principal == Agent::"b-only@1.0.0", action, resource)
      when { resource is Tool && resource.name == "repository.read" };
`

func writeBundles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestOneTenantsPermitAuthorizesNothingInAnother is VRT-AEON-005's acceptance criterion 3, written
// the way they asked for it: a negative test with a resource of the same name.
//
// THE SAME AGENT NAME IN BOTH TENANTS IS THE WHOLE POINT. The two bundles below both talk about
// Agent::"shared-name@1.0.0"-shaped identities, and A's permit must authorize nothing when the
// caller is from B — not because B's bundle forbids it, but because B's bundle is the only one
// consulted. That is the difference between a bundle per tenant and the tenant inside the policies:
// here there is no `when` clause to forget.
func TestOneTenantsPermitAuthorizesNothingInAnother(t *testing.T) {
	dir := writeBundles(t, map[string]string{"tenant-a.yaml": bundleA, "tenant-b.yaml": bundleB})
	set, err := LoadSetFromDir(dir)
	if err != nil {
		t.Fatalf("LoadSetFromDir: %v", err)
	}

	engineA, ok := set.EngineFor("tenant-a")
	if !ok {
		t.Fatal("tenant-a has no engine")
	}
	engineB, ok := set.EngineFor("tenant-b")
	if !ok {
		t.Fatal("tenant-b has no engine")
	}

	// Negative control first: A's permit really does work in A, or everything below passes because
	// nothing is permitted anywhere.
	if d := engineA.IsAllowed("shared-name@1.0.0", "search.web"); !d.Allowed {
		t.Fatalf("A's own permit does not authorize in A (%+v) — this test would prove nothing", d)
	}

	t.Run("A's permit does not reach B, for the same agent and the same tool", func(t *testing.T) {
		if d := engineB.IsAllowed("shared-name@1.0.0", "search.web"); d.Allowed {
			t.Errorf("B authorized a call that only A's bundle permits, via %q", d.PolicyID)
		}
	})

	t.Run("and the reverse, so the isolation is not one-directional", func(t *testing.T) {
		if d := engineB.IsAllowed("b-only@1.0.0", "repository.read"); !d.Allowed {
			t.Fatalf("B's own permit does not work (%+v)", d)
		}
		if d := engineA.IsAllowed("b-only@1.0.0", "repository.read"); d.Allowed {
			t.Errorf("A authorized a call that only B's bundle permits, via %q", d.PolicyID)
		}
	})
}

// TestATenantWithNoBundleIsDeniedAndNotDefaulted fixes the property the whole shape rests on.
//
// Returning some default engine for an unconfigured tenant would be the convenient thing and would
// make it inherit another tenant's permits — the same defect A2A-002 and RUN-006 each measured once,
// where a resource landed inside a permit nobody wrote for it. And it would look like a working
// deployment, which is why this is a test and not a comment.
func TestATenantWithNoBundleIsDeniedAndNotDefaulted(t *testing.T) {
	dir := writeBundles(t, map[string]string{"tenant-a.yaml": bundleA})
	set, err := LoadSetFromDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, tenant := range []string{"tenant-b", "default", ""} {
		if _, ok := set.EngineFor(tenant); ok {
			t.Errorf("tenant %q got an engine it has no bundle for", tenant)
		}
	}
	if _, ok := set.EngineFor("tenant-a"); !ok {
		t.Fatal("tenant-a lost its engine — negative control")
	}

	t.Run("a nil set authorizes nobody rather than panicking", func(t *testing.T) {
		var nilSet *Set
		if _, ok := nilSet.EngineFor("tenant-a"); ok {
			t.Error("a nil set handed out an engine")
		}
	})
}

// TestAMalformedPolicyDirectoryStopsTheGateway covers the startup refusals. Each one is a deployment
// that would otherwise serve policies nobody can account for.
func TestAMalformedPolicyDirectoryStopsTheGateway(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			// A stray file is refused rather than skipped, for the reason the migration runner gives
			// about the same choice: a bundle somebody believes is loaded and is not is worse than
			// one that is plainly missing. `policy_bundle.yaml` is the tempting case — it is what
			// the single-bundle deployments are called.
			name:  "a file that is not <tenant>.yaml",
			files: map[string]string{"policy_bundle.yaml": bundleA},
			want:  "is not named <tenant>.yaml",
		},
		{
			name:  "a directory with no bundle at all",
			files: map[string]string{},
			want:  "holds no <tenant>.yaml bundle",
		},
		{
			name:  "a bundle that is not valid Cedar",
			files: map[string]string{"tenant-a.yaml": "apiVersion: harness.ai/v1\nkind: PolicyBundle\npolicies:\n  - id: broken\n    effect: permit\n    cedarSource: \"this is not cedar\"\n"},
			want:  "loading",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadSetFromDir(writeBundles(t, tc.files))
			if err == nil {
				t.Fatal("the directory loaded without complaint")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v\nwant it to mention %q", err, tc.want)
			}
		})
	}
}

// TestTheSingleBundleDeploymentNowNamesItsTenant is what keeps an existing deployment honest.
//
// One bundle used to serve whoever called. The same file now serves exactly one NAMED tenant, and a
// caller from any other is denied — the policies are unchanged, the statement about who they apply
// to is the part that was implicit and is now written down.
func TestTheSingleBundleDeploymentNowNamesItsTenant(t *testing.T) {
	dir := writeBundles(t, map[string]string{"tenant-a.yaml": bundleA})
	set, err := LoadSetForSingleTenant("only-me", filepath.Join(dir, "tenant-a.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set.EngineFor("only-me"); !ok {
		t.Fatal("the named tenant has no engine")
	}
	// The FILE NAME does not decide the tenant here — the argument does. Worth fixing: a reader
	// could reasonably expect tenant-a.yaml to serve tenant-a, and in this mode it does not.
	if _, ok := set.EngineFor("tenant-a"); ok {
		t.Error("the file's name was used as the tenant; in single-bundle mode the tenant is the argument")
	}
	if got := set.String(); got != "only-me" {
		t.Errorf("startup line says %q, want only-me", got)
	}
}
