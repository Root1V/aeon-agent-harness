package toolexec

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ArtifactIDPattern is the shape of an artifact id, and the only thing artifact.read will open.
//
// WHY A SHAPE CHECK WHEN os.Root ALREADY CONTAINS EVERYTHING. It is not the security boundary — that
// is os.Root, and it holds on its own. This is here because artifact ids have a KNOWN shape and a
// caller sending anything else is not reaching for a file, it is reaching for a path. `obs_a1b2…` is
// something a run produced; `../secrets` is somebody trying. Both get refused either way, and only one
// of them deserves an error that says "that is not an artifact id".
//
// The two prefixes are the two writers: `art_` is what write_artifact_activity produces for a run's
// own output, `obs_` is what CTX-003's offload store has used since it was written
// (python/aeon_context/offload.py). Accepting both means this tool reads artifacts from either half
// without the id having to say which.
var ArtifactIDPattern = regexp.MustCompile(`^(art|obs)_[0-9a-f]{8,64}$`)

// RegisterArtifactReadTool wires `artifact.read`: content a run produced, fetched back by its id.
//
// THE OTHER UNIMPLEMENTED PROMISE. `artifact.read` has been in examples/deep-research/policy_bundle.yaml
// and in the agent manifest's `tools.allow` since those files were written, alongside
// `repository.read`, with nothing behind it — TOOL-008 closed the first and named this one as still
// open. So the manifest has been declaring two tools the gateway answered `unknown tool` for.
//
// WHAT AN ARTIFACT IS HERE, stated because nothing in the repo defined it before this. It is content a
// run produced and parked OUTSIDE its own context and outside Temporal's history: a finished report, a
// large tool result offloaded by CTX-003. Both are things that exist today and that nothing could read
// back. The design CTX-005 wrote down — "addressable recall… what lets a later tool call fetch the full
// content back by recall_id" — is exactly this tool, and it was DONE as a pure Python module with no
// caller, which is the state MEM-003 was in before MEM-003b.
//
// WHY IT IS NOT A SECOND repository.read POINTED SOMEWHERE ELSE. Same reader underneath (see
// readContainedTextFile — one copy of the containment, deliberately), and a different contract above
// it: the argument is an id and not a path, so there is no directory structure for a caller to walk and
// no filename to guess. A run can fetch what it produced and cannot enumerate what anyone else did.
// WHAT THIS DID NOT DO UNTIL GOV-001g, and it is worth reading before the code. It was pointed at ONE
// directory for the whole deployment, and an artifact id is
// `art_ + uuid5(FIXED_NAMESPACE, "<run_id>/<name>").hex` — a DETERMINISTIC function of a run id and a
// name, with the namespace a constant in this repository and the name a literal ("report.md" for every
// Deep Research run). So the sentence above — "a run can fetch what it produced and cannot enumerate
// what anyone else did" — was true about ENUMERATION and false about everything else: a tenant that
// had seen another tenant's run id could compute the id of its finished report and read it.
//
// Measured on 2026-10-08 before the fix, through the real gateway with real Cedar bundles: a caller
// in tenant-a asking for the id derived from a tenant-b run got
// `{"allowed":true,"policy_id":"allow-artifact-read","result":{"content":"<tenant B's report>"}}`.
//
// AND OBJECT STORAGE WOULD NOT HAVE FIXED IT, which is why this landed with the decision to delete
// MinIO from the compose rather than implement an S3 backend for it: a flat bucket with the same
// deterministic keys has exactly the same hole. The defect was the single namespace, not the medium.
//
// THE FIX IS CONTAINMENT AND NOT A PREDICATE. The tool is handed a RESOLVER and gets a *per-tenant*
// os.Root, so another tenant's artifact is not something it is allowed to open and declines — it is
// outside the root it can address at all. There is no check to forget, which is the property
// `engineFor` already has for Cedar bundles, and it is strictly stronger than comparing a tenant
// field inside the handler.
func RegisterArtifactReadTool(e *Executor, rootFor func(tenant string) (*os.Root, error), rootPath string) {
	e.Register("artifact.read", func(tenant string, args map[string]any) (map[string]any, error) {
		id, _ := args["artifact_id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, fmt.Errorf("toolexec: artifact.read: missing required string arg %q", "artifact_id")
		}
		if !ArtifactIDPattern.MatchString(id) {
			// The id is echoed back because a caller that got it from a run result needs to see WHICH id
			// was rejected — and because the most likely cause is a model inventing one, which is worth
			// being able to spot in a trace rather than reading as a missing file.
			return nil, fmt.Errorf(
				"toolexec: artifact.read: %q is not an artifact id (expected art_<hex> or obs_<hex>) — "+
					"artifacts are fetched by the id a run reported, not by path", id)
		}
		if tenant == "" {
			// No tenant means the gateway could not resolve one, and there is no "shared" artifact
			// store to fall back to. Refused rather than served from a deployment-wide directory,
			// which is the behaviour this feature removed.
			return nil, fmt.Errorf("toolexec: artifact.read: no tenant for this call, so there is no " +
				"artifact store to read from — artifacts are scoped to the tenant of the run that wrote them")
		}
		root, err := rootFor(tenant)
		if err != nil {
			return nil, fmt.Errorf("toolexec: artifact.read: opening the artifact store for tenant %s: %w", tenant, err)
		}
		defer root.Close()
		return readContainedTextFile(root, "artifact.read", "artifact_id", id, args)
	})
}
