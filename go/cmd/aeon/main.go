// Command aeon is the platform CLI (DX-002): init/validate/run/eval/trace/replay/publish.
//
// STATUS: only `validate` is implemented, and only against the JSON Schemas in proto/ — it does
// not yet talk to the control plane. The remaining subcommands are stubs that print what they will
// do — see roadmap.md DX-002, `TODO`.
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "validate":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: aeon validate <path-to-agent-manifest.yaml|.json>")
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

// validate does a minimal structural check: the file must be valid JSON (YAML manifests are
// expected to be converted to JSON before this ships — see roadmap.md DX-002) and must declare
// apiVersion "harness.ai/v1" and kind "Agent", matching proto/manifests/agent_manifest.schema.json.
// Full JSON Schema validation (required fields, enums) is TODO; this is a placeholder that at
// least prevents an obviously malformed manifest from being treated as valid.
func validate(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("not valid JSON (note: YAML manifest support is TODO): %w", err)
	}
	if doc["apiVersion"] != "harness.ai/v1" {
		return fmt.Errorf("apiVersion must be harness.ai/v1")
	}
	if doc["kind"] != "Agent" {
		return fmt.Errorf("kind must be Agent")
	}
	return nil
}
