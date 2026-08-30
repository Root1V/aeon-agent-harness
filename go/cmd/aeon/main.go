// Command aeon is the platform CLI (DX-002): init/validate/run/eval/trace/replay/publish.
//
// STATUS: `validate` (real JSON Schema validation, FND-003) and `eval list` (EVAL-001) are
// implemented. Neither talks to the control plane yet. The remaining subcommands are stubs that
// print what they will do — see roadmap.md DX-002, `TODO`.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	case "eval":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: aeon eval <list> [args]")
			os.Exit(1)
		}
		if os.Args[2] != "list" {
			fmt.Printf("aeon eval %s: not yet implemented — see roadmap.md EVAL-002/EVAL-003\n", os.Args[2])
			os.Exit(2)
		}
		if err := runEvalList(os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "eval list: %v\n", err)
			os.Exit(1)
		}
	case "init", "run", "trace", "replay", "publish":
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

// resolveEvalsDir finds the evals/ directory (containing suites/ and datasets/), mirroring
// resolveProtoDir's search order: AEON_EVALS_DIR env var, then ./evals, then ../evals, ../../evals.
func resolveEvalsDir() (string, error) {
	if dir := os.Getenv("AEON_EVALS_DIR"); dir != "" {
		return dir, nil
	}
	for _, candidate := range []string{"evals", filepath.Join("..", "evals"), filepath.Join("..", "..", "evals")} {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not locate evals/ (suites+datasets) — set AEON_EVALS_DIR explicitly")
}

// evalSuiteSummary is what `aeon eval list` shows for one registered EvalSuite (EVAL-001) —
// enough to see what a suite gates on and what it takes to pass, without opening the file.
type evalSuiteSummary struct {
	Name       string
	Version    string
	Dataset    string
	Graders    []string
	Thresholds map[string]float64
	GateOn     []string
}

// listEvalSuites reads every *.yaml/*.yml file in evalsDir/suites, validates it as a real EvalSuite
// against proto/manifests/eval_suite.schema.json (config-as-code, same principle as FND-003's
// AgentManifest — a suite that doesn't validate is a hard error, not silently skipped), and returns
// one summary per suite sorted by name.
func listEvalSuites(evalsDir, protoDir string) ([]evalSuiteSummary, error) {
	suitesDir := filepath.Join(evalsDir, "suites")
	entries, err := os.ReadDir(suitesDir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", suitesDir, err)
	}

	compiler := jsonschema.NewCompiler()
	if _, err := registerSchemaDir(compiler, filepath.Join(protoDir, "schemas")); err != nil {
		return nil, fmt.Errorf("loading proto/schemas: %w", err)
	}
	manifestIDs, err := registerSchemaDir(compiler, filepath.Join(protoDir, "manifests"))
	if err != nil {
		return nil, fmt.Errorf("loading proto/manifests: %w", err)
	}
	schemaID, ok := manifestIDs["eval_suite.schema.json"]
	if !ok {
		return nil, fmt.Errorf("internal error: eval_suite.schema.json was not registered (no $id?)")
	}
	schema, err := compiler.Compile(schemaID)
	if err != nil {
		return nil, fmt.Errorf("compiling eval_suite.schema.json: %w", err)
	}

	var summaries []evalSuiteSummary
	for _, e := range entries {
		if e.IsDir() || !(strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml")) {
			continue
		}
		summary, err := loadEvalSuiteSummary(filepath.Join(suitesDir, e.Name()), schema)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		summaries = append(summaries, summary)
	}

	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Name < summaries[j].Name })
	return summaries, nil
}

func loadEvalSuiteSummary(path string, schema *jsonschema.Schema) (evalSuiteSummary, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return evalSuiteSummary{}, err
	}

	var parsed any
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return evalSuiteSummary{}, fmt.Errorf("parsing: %w", err)
	}
	doc, ok := parsed.(map[string]any)
	if !ok {
		return evalSuiteSummary{}, fmt.Errorf("root must be a mapping/object")
	}

	docJSON, err := json.Marshal(doc)
	if err != nil {
		return evalSuiteSummary{}, fmt.Errorf("re-encoding as JSON: %w", err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(docJSON))
	if err != nil {
		return evalSuiteSummary{}, fmt.Errorf("decoding for validation: %w", err)
	}
	if err := schema.Validate(instance); err != nil {
		return evalSuiteSummary{}, fmt.Errorf("invalid EvalSuite: %w", err)
	}

	metadata, _ := doc["metadata"].(map[string]any)
	spec, _ := doc["spec"].(map[string]any)
	thresholds := map[string]float64{}
	if t, ok := spec["thresholds"].(map[string]any); ok {
		for k, v := range t {
			if f, ok := v.(float64); ok {
				thresholds[k] = f
			}
		}
	}

	return evalSuiteSummary{
		Name:       fmt.Sprint(metadata["name"]),
		Version:    fmt.Sprint(metadata["version"]),
		Dataset:    fmt.Sprint(spec["dataset"]),
		Graders:    toStringSlice(spec["graders"]),
		Thresholds: thresholds,
		GateOn:     toStringSlice(spec["gateOn"]),
	}, nil
}

func toStringSlice(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

// printEvalSuites is `aeon eval list`'s actual output — EVAL-001's acceptance criterion ("aeon eval
// list muestra las suites de evals/suites") is about what a human running this command sees, not
// just the parsed data structure.
func printEvalSuites(w io.Writer, suites []evalSuiteSummary) {
	for _, s := range suites {
		fmt.Fprintf(w, "%s\t%s\tdataset=%s\tgraders=%s\tthresholds=%v\tgateOn=%s\n",
			s.Name, s.Version, s.Dataset, strings.Join(s.Graders, ","), s.Thresholds, strings.Join(s.GateOn, ","))
	}
}

func runEvalList(w io.Writer) error {
	evalsDir, err := resolveEvalsDir()
	if err != nil {
		return err
	}
	protoDir, err := resolveProtoDir()
	if err != nil {
		return err
	}
	suites, err := listEvalSuites(evalsDir, protoDir)
	if err != nil {
		return err
	}
	printEvalSuites(w, suites)
	return nil
}
