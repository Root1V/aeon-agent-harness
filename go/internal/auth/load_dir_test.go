package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCallerFile puts one CallerBundle in dir under the given file name.
func writeCallerFile(t *testing.T, dir, name string, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

// callerDoc is one tenant's file: a caller whose token is derived from its id, so a test can
// authenticate with "token-<id>" and a test that wants a COLLISION can ask for one explicitly.
func callerDoc(tenant, id, tokenSeed string) string {
	return "kind: CallerBundle\ncallers:\n" +
		"  - id: " + id + "\n" +
		"    kind: service\n" +
		"    tenant: " + tenant + "\n" +
		"    tokenSHA256: \"" + HashToken("token-"+tokenSeed) + "\"\n"
}

// TestOneFilePerTenantIsABoundaryAndNotAFilingConvention is AEON_CALLERS_DIR's acceptance test.
//
// WHY IT EXISTS, and it came from the consumer rather than from us. Veritium closed VRT-AEON-005
// after verifying GOV-001f live, then asked how to hand us their shared-deployment configuration.
// Their policy bundle was a clean pull request — one file, named after their tenant. Their CALLER
// entry was not: callers loaded from ONE file, so adding a consumer meant editing a document
// holding every other tenant's callers, token hashes included.
//
// The rules below are what makes a directory a boundary instead of a filing convention, and each
// one is here because without it a file could reach outside its own tenant.
func TestOneFilePerTenantIsABoundaryAndNotAFilingConvention(t *testing.T) {
	t.Run("two tenants' files load as one bundle and each authenticates its own", func(t *testing.T) {
		dir := t.TempDir()
		writeCallerFile(t, dir, "veritium.yaml", callerDoc("veritium", "veritium-api", "veritium-api"))
		writeCallerFile(t, dir, "otro.yml", callerDoc("otro", "otro-api", "otro-api"))

		a, err := LoadDir(dir)
		if err != nil {
			t.Fatalf("LoadDir: %v", err)
		}
		// .yaml AND .yml, because the pattern accepts both and a test that only used one would not
		// say whether that was deliberate.
		for _, want := range []struct{ token, id, tenant string }{
			{"token-veritium-api", "veritium-api", "veritium"},
			{"token-otro-api", "otro-api", "otro"},
		} {
			caller, err := a.Authenticate(want.token)
			if err != nil {
				t.Fatalf("Authenticate(%s): %v", want.id, err)
			}
			if caller.ID != want.id || caller.Tenant != want.tenant {
				t.Errorf("got caller %q in tenant %q, want %q in %q", caller.ID, caller.Tenant, want.id, want.tenant)
			}
		}
	})

	t.Run("a file may not declare a caller in another tenant", func(t *testing.T) {
		// THE RULE THAT MAKES THE FILE NAME MEAN SOMETHING. A policy bundle governs one tenant and
		// names none inside, so the file name is enough on its own. A caller DECLARES its tenant, so
		// without this check veritium.yaml could add a caller in tenant `otro` — and the directory
		// would be a filing convention rather than the boundary it exists to be.
		dir := t.TempDir()
		writeCallerFile(t, dir, "veritium.yaml", callerDoc("otro", "sneaky", "sneaky"))

		_, err := LoadDir(dir)
		if err == nil {
			t.Fatal("veritium.yaml declared a caller in tenant `otro` and loaded")
		}
		for _, want := range []string{"veritium", "otro", "sneaky"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %q, so an operator cannot see which file claimed which "+
					"tenant: %v", want, err)
			}
		}
	})

	t.Run("one token in two files is refused", func(t *testing.T) {
		// THE COLLISION THAT WILL ACTUALLY HAPPEN, and the reason the directory is merged and then
		// validated rather than validated file by file: two teams do not see each other's file. Checked
		// per file this passes twice, and which identity a request gets would be decided by the
		// filesystem's read order.
		dir := t.TempDir()
		shared := "shared-secret"
		writeCallerFile(t, dir, "veritium.yaml", callerDoc("veritium", "veritium-api", shared))
		writeCallerFile(t, dir, "otro.yaml", callerDoc("otro", "otro-api", shared))

		_, err := LoadDir(dir)
		if err == nil {
			t.Fatal("two tenants sharing a token hash loaded. One identity would answer to both names, " +
				"and which one is whatever the directory listed first")
		}
	})

	t.Run("a duplicate caller id across files names both files", func(t *testing.T) {
		dir := t.TempDir()
		writeCallerFile(t, dir, "veritium.yaml", callerDoc("veritium", "api", "one"))
		writeCallerFile(t, dir, "otro.yaml", callerDoc("otro", "api", "two"))

		_, err := LoadDir(dir)
		if err == nil {
			t.Fatal("the same caller id in two files loaded")
		}
		// Both file names, because the whole point is that the operator owns one of them and has to be
		// told which other one they collided with.
		for _, want := range []string{"veritium.yaml", "otro.yaml"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %s: %v", want, err)
			}
		}
	})

	t.Run("a stray file is refused, not skipped", func(t *testing.T) {
		// Including a README, which is the innocent-looking case. The alternative is a
		// `veritium.yaml.bak` silently ignored — a file somebody believes is loaded and is not.
		//
		// EACH ONE ALONE IN ITS OWN DIRECTORY, and that is not tidiness. The first version put a valid
		// `veritium.yaml` beside each stray, and `Veritium.yaml` then OVERWROTE it: macOS is
		// case-insensitive, so the test measured its own fixture — the error it got was a YAML parse
		// failure on "whatever", not the stray-file refusal it was asserting. A test that writes two
		// names differing only in case is not portable, and on this machine it quietly tested
		// something else.
		for _, stray := range []string{"README.md", "veritium.yaml.bak", "Veritium.yaml", "callers.json", "veritium.txt"} {
			dir := t.TempDir()
			writeCallerFile(t, dir, stray, "whatever")

			if _, err := LoadDir(dir); err == nil {
				t.Errorf("a directory containing %s loaded", stray)
			} else if !strings.Contains(err.Error(), stray) {
				t.Errorf("the error for %s does not name the file: %v", stray, err)
			}
		}

		// And a valid file does not excuse a stray beside it, which is the case that matters in a
		// shared directory: one team's correct file must not make another's typo invisible.
		dir := t.TempDir()
		writeCallerFile(t, dir, "veritium.yaml", callerDoc("veritium", "veritium-api", "veritium-api"))
		writeCallerFile(t, dir, "README.md", "notes for whoever adds a tenant")
		if _, err := LoadDir(dir); err == nil {
			t.Error("a directory with one valid file and a README loaded")
		}
	})

	t.Run("an empty directory and an empty file are both refused", func(t *testing.T) {
		// A service with no callers starts, looks healthy and authenticates nobody, which is the
		// failure MustLoadFromEnv's own comment calls worse than not starting.
		if _, err := LoadDir(t.TempDir()); err == nil {
			t.Error("an empty callers directory loaded")
		}
		dir := t.TempDir()
		writeCallerFile(t, dir, "veritium.yaml", "kind: CallerBundle\ncallers: []\n")
		if _, err := LoadDir(dir); err == nil {
			t.Error("a file declaring no callers loaded — somebody wrote that file expecting it to do something")
		}
	})

	t.Run("every rule Load already enforces still holds across the directory", func(t *testing.T) {
		// The reason LoadDir merges rather than reimplements: a caller that is not `service` may not
		// hold mayActForTenants, and that must be true of a caller arriving from a per-tenant file too.
		dir := t.TempDir()
		writeCallerFile(t, dir, "veritium.yaml", "kind: CallerBundle\ncallers:\n"+
			"  - id: human-with-privilege\n    kind: human\n    tenant: veritium\n"+
			"    tokenSHA256: \""+HashToken("t")+"\"\n    mayActForTenants: [otro]\n")
		_, err := LoadDir(dir)
		if err == nil {
			t.Fatal("a human caller with mayActForTenants loaded from a per-tenant file")
		}
		if !strings.Contains(err.Error(), string(KindService)) {
			t.Errorf("the error does not say which kind may hold it: %v", err)
		}
	})
}

// TestTheTwoCallerSourcesAreNotAPrecedenceQuestion covers the startup refusals. fatal is a package
// variable precisely so these are testable rather than an os.Exit nobody exercises.
func TestTheTwoCallerSourcesAreNotAPrecedenceQuestion(t *testing.T) {
	original := fatal
	t.Cleanup(func() { fatal = original })

	var got string
	fatal = func(format string, args ...any) {
		got = fmt.Sprintf(format, args...)
		panic(sentinel{})
	}
	call := func(t *testing.T, path, dir string) string {
		t.Helper()
		t.Setenv(CallersPathEnv, path)
		t.Setenv(CallersDirEnv, dir)
		got = ""
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(sentinel); !ok {
					panic(r)
				}
			}
		}()
		MustLoadFromEnv("test-service")
		return ""
	}

	t.Run("both set is refused rather than one winning", func(t *testing.T) {
		call(t, "/some/callers.yaml", "/some/dir")
		if got == "" {
			t.Fatal("setting both was accepted. Whichever lost would be a file an operator believes is " +
				"loaded and is not")
		}
		for _, want := range []string{CallersPathEnv, CallersDirEnv} {
			if !strings.Contains(got, want) {
				t.Errorf("the refusal does not name %s: %s", want, got)
			}
		}
	})

	t.Run("neither set names both variables", func(t *testing.T) {
		call(t, "", "")
		if got == "" {
			t.Fatal("a service with no caller bundle at all started")
		}
		if !strings.Contains(got, CallersDirEnv) {
			t.Errorf("the refusal does not mention the directory option, so an operator reading it does "+
				"not learn it exists: %s", got)
		}
	})
}

type sentinel struct{}

// TestTheShippedExampleDirectoryLoads asserts on the CHECKED-IN directory and not on a fixture.
//
// Same discipline the policy tests apply to the committed bundle: what can regress is the file a
// deployment actually mounts. And it is what keeps the directory mode exercised by the reference
// stack rather than only by the tests above. examples/callers/tenants/default.yaml declares the SAME
// THREE CALLERS with the same hashes as the single bundle — not the same bytes, it carries its own
// header — so a deployment can switch AEON_CALLERS_PATH for AEON_CALLERS_DIR and nothing about its
// behaviour changes.
func TestTheShippedExampleDirectoryLoads(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "examples", "callers", "tenants")
	a, err := LoadDir(dir)
	if err != nil {
		// NOT a skip: the directory is in this repository, so an unreadable one means it moved and
		// the thing a deployment mounts has stopped being checked.
		t.Fatalf("LoadDir(%s): %v", dir, err)
	}

	// The three development callers the single bundle declares, with the properties that matter.
	for _, want := range []struct {
		token, id  string
		mayApprove bool
	}{
		{"dev-worker-token-not-a-secret", "worker", false},
		{"dev-operator-token-not-a-secret", "operator", true},
	} {
		caller, err := a.Authenticate(want.token)
		if err != nil {
			t.Errorf("Authenticate(%s): %v", want.id, err)
			continue
		}
		if caller.ID != want.id {
			t.Errorf("token for %s authenticated as %q", want.id, caller.ID)
		}
		if caller.Tenant != "default" {
			t.Errorf("caller %q is in tenant %q, want default", caller.ID, caller.Tenant)
		}
		// The asymmetry A5 depends on: the worker is the process that gets blocked on an approval, so
		// a credential that could both run the tool and approve it would make the gate a formality.
		if caller.MayApprove != want.mayApprove {
			t.Errorf("caller %q has mayApprove=%v, want %v", caller.ID, caller.MayApprove, want.mayApprove)
		}
	}
}
