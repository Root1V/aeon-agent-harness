// Command aeon is the platform CLI (DX-002): init/validate/run/eval/trace/replay/publish.
//
// STATUS: only `validate` is implemented — real JSON Schema validation (FND-003) against
// proto/manifests/*.schema.json, with cross-file $ref resolution into proto/schemas/*.schema.json.
// It does not yet talk to the control plane. The remaining subcommands are stubs that print what
// they will do — see roadmap.md DX-002, `TODO`.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// manifestSchemaFile maps a manifest's `kind` to its schema file under proto/manifests/. FND-003:
// config-as-code means these four kinds — not the UI — are the source of truth for what's valid.
var manifestSchemaFile = map[string]string{
	"Agent":             "agent_manifest.schema.json",
	"EvalSuite":         "eval_suite.schema.json",
	"PolicyBundle":      "policy_bundle.schema.json",
	"ModelPolicyBundle": "model_policy_bundle.schema.json",
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "validate":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: aeon validate <path-to-manifest.yaml|.json>")
			os.Exit(1)
		}
		if err := validate(os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "invalid: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("valid")
	case "init", "run", "eval", "trace", "replay", "publish":
		fmt.Printf("aeon %s: not yet implemented — see roadmap.md DX-002\n", os.Args[1])
		os.Exit(2)
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: aeon <init|validate|run|eval|trace|replay|publish> [args]")
}

// validate parses a manifest (YAML or JSON — YAML is a superset, so one path handles both),
// picks its schema by `kind`, and validates it with full JSON Schema semantics: required fields,
// enums (including apiVersion/kind's `const`), and cross-file $refs (model_policy_bundle.schema.json
// -> model_profile.schema.json) resolved offline from proto/schemas/, no network access.
func validate(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var parsed any
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("parsing manifest: %w", err)
	}
	doc, ok := parsed.(map[string]any)
	if !ok {
		return fmt.Errorf("manifest root must be a mapping/object")
	}

	kind, _ := doc["kind"].(string)
	schemaFile, ok := manifestSchemaFile[kind]
	if !ok {
		known := make([]string, 0, len(manifestSchemaFile))
		for k := range manifestSchemaFile {
			known = append(known, k)
		}
		return fmt.Errorf("unknown or missing 'kind' %q — expected one of %v", kind, known)
	}

	protoDir, err := resolveProtoDir()
	if err != nil {
		return err
	}

	compiler := jsonschema.NewCompiler()
	if _, err := registerSchemaDir(compiler, filepath.Join(protoDir, "schemas")); err != nil {
		return fmt.Errorf("loading proto/schemas: %w", err)
	}
	manifestIDs, err := registerSchemaDir(compiler, filepath.Join(protoDir, "manifests"))
	if err != nil {
		return fmt.Errorf("loading proto/manifests: %w", err)
	}

	schemaID, ok := manifestIDs[schemaFile]
	if !ok {
		return fmt.Errorf("internal error: %s was not registered (no $id?)", schemaFile)
	}
	schema, err := compiler.Compile(schemaID)
	if err != nil {
		return fmt.Errorf("compiling schema %s: %w", schemaFile, err)
	}

	// Round-trip through JSON: jsonschema.Validate expects the number/type representation
	// produced by jsonschema.UnmarshalJSON, not whatever concrete types yaml.v3 happened to pick.
	docJSON, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("re-encoding manifest as JSON: %w", err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(docJSON))
	if err != nil {
		return fmt.Errorf("decoding manifest for validation: %w", err)
	}

	return schema.Validate(instance)
}

// registerSchemaDir registers every *.schema.json file in dir with the compiler under its own
// declared $id (so $refs between files resolve offline), and returns a filename -> $id map so
// callers can look up the id for a specific file they need to Compile().
func registerSchemaDir(c *jsonschema.Compiler, dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		var meta struct {
			ID string `json:"$id"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if meta.ID == "" {
			continue
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if err := c.AddResource(meta.ID, doc); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		ids[e.Name()] = meta.ID
	}
	return ids, nil
}

// resolveProtoDir finds the proto/ directory (containing schemas/ and manifests/) so `aeon
// validate` works both from a repo checkout and from a container with proto/ mounted at a fixed
// path. Priority: AEON_SCHEMAS_DIR env var, then ./proto, then ../proto (covers running from
// go/ or from the repo root, the two common cases in this repo's own Makefile/tests).
func resolveProtoDir() (string, error) {
	if dir := os.Getenv("AEON_SCHEMAS_DIR"); dir != "" {
		return dir, nil
	}
	for _, candidate := range []string{"proto", filepath.Join("..", "proto"), filepath.Join("..", "..", "proto")} {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not locate proto/ (schemas+manifests) — set AEON_SCHEMAS_DIR explicitly")
}
