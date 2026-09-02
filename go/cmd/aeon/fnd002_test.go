package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/abom"
)

// TestPublishGeneratesAndSignsReproducibleABOM is FND-002's acceptance test: `aeon publish` writes
// a real, Ed25519-signed ABOM next to the manifest, the signature verifies, and — given a fixed
// signing key (the real production path, AEON_ABOM_SIGNING_KEY) — publishing the identical
// manifest twice produces a byte-identical signed ABOM both times.
func TestPublishGeneratesAndSignsReproducibleABOM(t *testing.T) {
	root := repoRoot(t)
	withSchemasDir(t, filepath.Join(root, "proto"))
	srv := newFakeControlPlane(t)
	t.Setenv("AEON_CONTROLPLANE_ADDR", strings.TrimPrefix(srv.URL, "http://"))
	t.Setenv("AEON_ABOM_SIGNING_KEY", strings.Repeat("cd", 32)) // 32 bytes of 0xcd, a fixed test seed

	manifestPath := writeTempAgentManifest(t, "abom-test-agent", "0.1.0")

	var buf1 bytes.Buffer
	if err := runPublish(&buf1, manifestPath); err != nil {
		t.Fatalf("runPublish (1st): %v", err)
	}
	abomPath := extractABOMPath(t, buf1.String())
	firstBytes, err := os.ReadFile(abomPath)
	if err != nil {
		t.Fatalf("reading ABOM: %v", err)
	}

	var signed abom.Signed
	if err := json.Unmarshal(firstBytes, &signed); err != nil {
		t.Fatalf("decoding ABOM: %v", err)
	}
	if signed.Algorithm != abom.SignatureAlgorithm {
		t.Fatalf("signature_algorithm = %q, want %q", signed.Algorithm, abom.SignatureAlgorithm)
	}
	if signed.Document.Agent.Name != "abom-test-agent" || signed.Document.Agent.Version != "0.1.0" {
		t.Fatalf("unexpected AgentRef in ABOM: %+v", signed.Document.Agent)
	}
	ok, err := abom.Verify(signed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Fatal("expected the published ABOM's signature to verify")
	}

	// Re-publish the identical manifest with the same signing key: the whole signed ABOM file must
	// come out byte-for-byte identical — the real, checkable meaning of "reproducible" here.
	var buf2 bytes.Buffer
	if err := runPublish(&buf2, manifestPath); err != nil {
		t.Fatalf("runPublish (2nd): %v", err)
	}
	secondBytes, err := os.ReadFile(extractABOMPath(t, buf2.String()))
	if err != nil {
		t.Fatalf("reading ABOM (2nd): %v", err)
	}
	if string(firstBytes) != string(secondBytes) {
		t.Fatalf("expected byte-identical ABOMs across two publishes of the same manifest with the same key:\n%s\nvs\n%s", firstBytes, secondBytes)
	}
}

func TestPublishWarnsWhenSigningKeyIsEphemeral(t *testing.T) {
	root := repoRoot(t)
	withSchemasDir(t, filepath.Join(root, "proto"))
	srv := newFakeControlPlane(t)
	t.Setenv("AEON_CONTROLPLANE_ADDR", strings.TrimPrefix(srv.URL, "http://"))
	t.Setenv("AEON_ABOM_SIGNING_KEY", "") // explicitly unset

	manifestPath := writeTempAgentManifest(t, "abom-ephemeral-agent", "0.1.0")

	var buf bytes.Buffer
	if err := runPublish(&buf, manifestPath); err != nil {
		t.Fatalf("runPublish: %v", err)
	}
	if !strings.Contains(buf.String(), "ephemeral") {
		t.Errorf("expected a warning about the ephemeral signing key, got: %q", buf.String())
	}

	abomPath := extractABOMPath(t, buf.String())
	raw, err := os.ReadFile(abomPath)
	if err != nil {
		t.Fatalf("reading ABOM: %v", err)
	}
	var signed abom.Signed
	if err := json.Unmarshal(raw, &signed); err != nil {
		t.Fatalf("decoding ABOM: %v", err)
	}
	ok, err := abom.Verify(signed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Fatal("expected even an ephemeral-key ABOM to self-verify")
	}
}

// extractABOMPath pulls the "abom=<path>" token runPublish prints on success.
func extractABOMPath(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "abom=") {
			fields := strings.Fields(line)
			return strings.TrimPrefix(fields[0], "abom=")
		}
	}
	t.Fatalf("no abom= line found in output: %q", output)
	return ""
}
