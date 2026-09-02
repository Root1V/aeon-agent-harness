package abom

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func testManifest(t *testing.T) (map[string]any, []byte) {
	t.Helper()
	raw := []byte("apiVersion: harness.ai/v1\nkind: Agent\nmetadata:\n  name: test-agent\n  version: 0.1.0\n  owner: test-owner\n")
	manifest := map[string]any{
		"metadata": map[string]any{"name": "test-agent", "version": "0.1.0", "owner": "test-owner"},
		"spec": map[string]any{
			"modelPolicy": map[string]any{"profile": "reasoning-high", "fallbacks": []any{"reasoning-balanced"}},
			"tools":       map[string]any{"allow": []any{"search.web"}, "deny": []any{"shell.exec"}},
			"evalGates":   []any{"deep_research_core"},
		},
	}
	return manifest, raw
}

func TestBuildDocumentIsDeterministic(t *testing.T) {
	manifest, raw := testManifest(t)
	d1, err := BuildDocument(manifest, raw)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	d2, err := BuildDocument(manifest, raw)
	if err != nil {
		t.Fatalf("BuildDocument (2nd call): %v", err)
	}
	b1, _ := d1.CanonicalBytes()
	b2, _ := d2.CanonicalBytes()
	if string(b1) != string(b2) {
		t.Fatalf("BuildDocument produced different bytes for identical input:\n%s\nvs\n%s", b1, b2)
	}
	if d1.Agent.Name != "test-agent" || d1.Agent.Version != "0.1.0" || d1.Agent.Owner != "test-owner" {
		t.Fatalf("unexpected AgentRef: %+v", d1.Agent)
	}
	if d1.ModelPolicy.Profile != "reasoning-high" || len(d1.ModelPolicy.Fallbacks) != 1 {
		t.Fatalf("unexpected ModelPolicyRef: %+v", d1.ModelPolicy)
	}
	if len(d1.Tools.Allow) != 1 || d1.Tools.Allow[0] != "search.web" || len(d1.Tools.Deny) != 1 {
		t.Fatalf("unexpected ToolsRef: %+v", d1.Tools)
	}
	if len(d1.EvalGates) != 1 || d1.EvalGates[0] != "deep_research_core" {
		t.Fatalf("unexpected EvalGates: %+v", d1.EvalGates)
	}
}

func TestBuildDocumentManifestSHA256MatchesRawBytes(t *testing.T) {
	manifest, raw := testManifest(t)
	d, err := BuildDocument(manifest, raw)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	// A single-byte change to the raw manifest must change the hash — this is what makes the ABOM
	// verify against the literal published file, not just its parsed fields.
	mutatedRaw := append([]byte(nil), raw...)
	mutatedRaw[0] = 'X'
	dMutated, err := BuildDocument(manifest, mutatedRaw)
	if err != nil {
		t.Fatalf("BuildDocument (mutated): %v", err)
	}
	if d.ManifestSHA256 == dMutated.ManifestSHA256 {
		t.Fatal("expected ManifestSHA256 to differ when the raw manifest bytes differ")
	}
}

func TestBuildDocumentRequiresNameAndVersion(t *testing.T) {
	if _, err := BuildDocument(map[string]any{}, []byte("x")); err == nil {
		t.Fatal("expected an error for a manifest with no metadata")
	}
}

func TestSignThenVerifySucceeds(t *testing.T) {
	manifest, raw := testManifest(t)
	doc, err := BuildDocument(manifest, raw)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	signed, err := Sign(doc, priv)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	ok, err := Verify(signed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Fatal("expected Verify to succeed for an untampered, correctly-signed document")
	}
}

// TestSignIsReproducible is FND-002's core claim, made concrete: the same document signed twice
// with the same key produces byte-identical signatures — Ed25519 (RFC 8032) is deterministic,
// unlike randomized schemes such as ECDSA.
func TestSignIsReproducible(t *testing.T) {
	manifest, raw := testManifest(t)
	doc, err := BuildDocument(manifest, raw)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	s1, err := Sign(doc, priv)
	if err != nil {
		t.Fatalf("Sign (1st): %v", err)
	}
	s2, err := Sign(doc, priv)
	if err != nil {
		t.Fatalf("Sign (2nd): %v", err)
	}
	if s1.Signature != s2.Signature {
		t.Fatalf("expected identical signatures for the same (document, key) pair, got %q vs %q", s1.Signature, s2.Signature)
	}
}

func TestVerifyRejectsATamperedDocument(t *testing.T) {
	manifest, raw := testManifest(t)
	doc, err := BuildDocument(manifest, raw)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	signed, err := Sign(doc, priv)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	signed.Document.Agent.Owner = "someone-else"
	ok, err := Verify(signed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ok {
		t.Fatal("expected Verify to fail for a document mutated after signing")
	}
}

func TestVerifyRejectsAWrongPublicKey(t *testing.T) {
	manifest, raw := testManifest(t)
	doc, err := BuildDocument(manifest, raw)
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	signed, err := Sign(doc, priv)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	_, otherPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating other key: %v", err)
	}
	otherPub, _ := otherPriv.Public().(ed25519.PublicKey)
	signed.PublicKey = base64.StdEncoding.EncodeToString(otherPub)

	ok, err := Verify(signed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ok {
		t.Fatal("expected Verify to fail when the embedded public key doesn't match the real signer")
	}
}

func TestLoadOrGenerateSigningKeyWithNoSeedIsEphemeral(t *testing.T) {
	priv, ephemeral, err := LoadOrGenerateSigningKey("")
	if err != nil {
		t.Fatalf("LoadOrGenerateSigningKey: %v", err)
	}
	if !ephemeral {
		t.Fatal("expected ephemeral=true when no seed is configured")
	}
	if len(priv) != ed25519.PrivateKeySize {
		t.Fatalf("unexpected private key size: %d", len(priv))
	}
}

func TestLoadOrGenerateSigningKeyWithSeedIsReproducibleAcrossProcesses(t *testing.T) {
	seed := strings.Repeat("ab", ed25519.SeedSize) // 32 bytes of 0xab, valid hex
	priv1, ephemeral1, err := LoadOrGenerateSigningKey(seed)
	if err != nil {
		t.Fatalf("LoadOrGenerateSigningKey (1st): %v", err)
	}
	priv2, ephemeral2, err := LoadOrGenerateSigningKey(seed)
	if err != nil {
		t.Fatalf("LoadOrGenerateSigningKey (2nd): %v", err)
	}
	if ephemeral1 || ephemeral2 {
		t.Fatal("expected ephemeral=false when a seed is configured")
	}
	if !priv1.Equal(priv2) {
		t.Fatal("expected the same hex seed to always derive the same private key")
	}
}

func TestLoadOrGenerateSigningKeyRejectsInvalidSeed(t *testing.T) {
	if _, _, err := LoadOrGenerateSigningKey("not-hex!!"); err == nil {
		t.Fatal("expected an error for non-hex input")
	}
	if _, _, err := LoadOrGenerateSigningKey(hex.EncodeToString([]byte("too-short"))); err == nil {
		t.Fatal("expected an error for a seed of the wrong length")
	}
}
