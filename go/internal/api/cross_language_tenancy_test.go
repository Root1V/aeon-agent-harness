package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/runcontroller"
)

// TestTheTenancyStringsMeanTheSameThingInBothLanguages reads python/aeon_worker/tenancy.py and
// compares it with the Go constants.
//
// WHY A TEST AND NOT A COMMENT. A memo key and a header name are plain strings crossing a language
// boundary with no schema between them, and a mismatch is SILENT in the worst possible way: the
// workflow reads an empty tenant, or the gateway ignores the header, and either way the system keeps
// working — having fallen back to the worker's own tenant, which is exactly the VRT-AEON-005 defect
// restored by a typo. Nothing fails, nothing is denied, and a shared deployment is judging every
// run against the wrong bundle again.
//
// Same reasoning as the golden-corpus drift test: a constant shared by two implementations and
// defined in neither needs something that actually compares the two.
func TestTheTenancyStringsMeanTheSameThingInBothLanguages(t *testing.T) {
	path := filepath.Join("..", "..", "..", "python", "aeon_worker", "tenancy.py")
	raw, err := os.ReadFile(path)
	if err != nil {
		// NOT a skip. The file is in this repository, so an unreadable one is a moved or deleted
		// module and the comparison this test exists to make has silently stopped happening.
		t.Fatalf("reading %s: %v — the Python side of these two constants is gone, so nothing is "+
			"checking that they still agree", path, err)
	}
	python := string(raw)

	for _, c := range []struct {
		pythonName string
		goName     string
		goValue    string
	}{
		{"TENANT_MEMO_KEY", "runcontroller.TenantMemoKey", runcontroller.TenantMemoKey},
		{"RUN_TENANT_HEADER", "api.RunTenantHeader", RunTenantHeader},
	} {
		want := c.pythonName + ` = "` + c.goValue + `"`
		if !strings.Contains(python, want) {
			t.Errorf("%s does not declare %s.\n%s = %q, so tenancy.py must contain:\n\t%s\n"+
				"A mismatch here does not fail anywhere: the tenant silently falls back to the "+
				"worker's own, which is the defect this propagation exists to fix",
				path, want, c.goName, c.goValue, want)
		}
	}
}
