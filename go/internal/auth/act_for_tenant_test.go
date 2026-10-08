package auth

import (
	"strings"
	"testing"
)

// The privilege a worker needs to execute somebody else's run, and the four ways a bundle that
// declares it wrong must fail to LOAD rather than be quietly ignored.
//
// Why loudly: a privilege that is silently dropped is indistinguishable from one that was granted,
// and the operator who wrote it believes it applies. Same argument the Tenant field already makes
// for refusing a default.
func TestACallerMayOnlyActForTenantsTheOperatorListed(t *testing.T) {
	bundle := func(c Caller) CallerBundleDoc {
		if c.TokenSHA256 == "" {
			c.TokenSHA256 = HashToken("token-" + c.ID)
		}
		return CallerBundleDoc{Kind: "CallerBundle", Callers: []Caller{c}}
	}
	// Through Authenticate rather than a lookup by id, so these assertions run over the Caller a real
	// request gets rather than over the document that was loaded.
	loaded := func(t *testing.T, c Caller) Caller {
		t.Helper()
		a, err := Load(bundle(c))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		got, err := a.Authenticate("token-" + c.ID)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		return got
	}

	t.Run("its own tenant needs no privilege at all", func(t *testing.T) {
		// This is what keeps every existing single-tenant deployment working with no configuration:
		// the worker names the tenant it already belongs to, and the header is never the thing that
		// breaks an install.
		c := loaded(t, Caller{ID: "w", Kind: KindService, Tenant: "veritium"})
		if !c.ActsForTenant("veritium") {
			t.Error("a caller may not name its own tenant, so a worker in a one-tenant deployment would be refused")
		}
		if c.ActsForTenant("someone-else") {
			t.Error("a caller with no mayActForTenants may name another tenant — the boundary is open by default")
		}
	})

	t.Run("a listed tenant is allowed and an unlisted one is not", func(t *testing.T) {
		c := loaded(t, Caller{
			ID: "worker", Kind: KindService, Tenant: "ops",
			MayActForTenants: []string{"tenant-a", "tenant-b"},
		})
		for _, allowed := range []string{"ops", "tenant-a", "tenant-b"} {
			if !c.ActsForTenant(allowed) {
				t.Errorf("may not act for %q, which is listed (or its own)", allowed)
			}
		}
		// The assertion that makes the list mean something. Without it the test would pass against a
		// function that returns true.
		if c.ActsForTenant("tenant-c") {
			t.Error("may act for tenant-c, which nobody listed")
		}
	})

	t.Run("a wildcard is refused, not interpreted", func(t *testing.T) {
		// The whole point of having no wildcard: `*` is "may act for every tenant", which is the
		// isolation boundary removed by a credential that still reads as configured. An operator who
		// writes it gets an error naming the reason, not a caller that works.
		for _, attempt := range []string{"*", "ALL", "tenant-*", "", "  ", "a*b"} {
			_, err := Load(bundle(Caller{
				ID: "w", Kind: KindService, Tenant: "ops", MayActForTenants: []string{attempt},
			}))
			if err == nil {
				t.Errorf("mayActForTenants: [%q] loaded", attempt)
				continue
			}
			if !strings.Contains(err.Error(), "mayActForTenants") && !strings.Contains(err.Error(), "may act for tenant") {
				t.Errorf("the error for %q does not name the field: %v", attempt, err)
			}
		}

		// AND "all" IS NOT A WILDCARD, which this subtest asserts rather than leaves to the regex.
		// It is a legal tenant name and it loads, granting exactly one tenant — the one called "all".
		// My first version of this test had "all" in the list above and failed, which is the useful
		// kind of failure: a reader who assumes the string is special would write the same bug into
		// the server, where it would read as a wildcard that works.
		c := loaded(t, Caller{
			ID: "w", Kind: KindService, Tenant: "ops", MayActForTenants: []string{"all"},
		})
		if !c.ActsForTenant("all") {
			t.Error("a tenant literally named \"all\" was not granted")
		}
		if c.ActsForTenant("tenant-a") {
			t.Fatal("\"all\" was interpreted as a wildcard — the isolation boundary is gone and the " +
				"bundle reads as if one tenant had been granted")
		}
	})

	t.Run("only a service caller may hold it", func(t *testing.T) {
		for _, kind := range []Kind{KindHuman, KindExternal} {
			_, err := Load(bundle(Caller{
				ID: "c", Kind: kind, Tenant: "ops", MayActForTenants: []string{"tenant-a"},
			}))
			if err == nil {
				t.Fatalf("a %s caller loaded with mayActForTenants", kind)
			}
			if !strings.Contains(err.Error(), string(KindService)) {
				t.Errorf("the error does not say which kind may: %v", err)
			}
		}
	})

	t.Run("a duplicate entry is refused", func(t *testing.T) {
		_, err := Load(bundle(Caller{
			ID: "w", Kind: KindService, Tenant: "ops", MayActForTenants: []string{"tenant-a", "tenant-a"},
		}))
		if err == nil {
			t.Fatal("a duplicated tenant loaded. It changes nothing at runtime, which is the reason to " +
				"refuse it: a list an operator cannot read back correctly is one they cannot audit")
		}
	})

	t.Run("its own tenant listed again is dropped, not an error", func(t *testing.T) {
		// Redundant rather than wrong, so the loaded list means "and these others" — which is what
		// an operator reading it back will assume.
		got := loaded(t, Caller{
			ID: "w", Kind: KindService, Tenant: "ops", MayActForTenants: []string{"ops", "tenant-a"},
		}).MayActForTenants
		if len(got) != 1 || got[0] != "tenant-a" {
			t.Fatalf("MayActForTenants = %v, want just [tenant-a]", got)
		}
	})
}
