// Package abom builds and signs an Agent Bill of Materials (FND-002): a reproducible, verifiable
// record of exactly what "aeon publish" published — the agent's identity, the exact manifest bytes
// published (by hash), and every governed reference the manifest itself declares (model policy
// profile/fallbacks, allowed/denied tools, eval gates). "Reproducible" is a real, checkable
// property here, not marketing: Document contains no timestamp or other non-deterministic field, so
// the same manifest bytes always produce byte-identical canonical JSON — and Ed25519 signatures are
// themselves deterministic (RFC 8032: same key + same message always yields the same signature
// bytes, unlike ECDSA's randomized signing), so signing the same Document twice with the same key
// produces an identical Signed envelope, byte for byte.
//
// Scope, real and deliberately bounded (see backlog.md): the Tool/Skill entries below are the names
// the manifest itself lists (spec.tools.allow/deny) — not full ToolDescriptor records resolved from
// the Tool Registry, since `aeon` the CLI has no Tool Registry client today. Full per-tool signing
// (ASI04, named in the architecture doc) needs that resolution first.
package abom

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Version identifies this document's shape, so a future incompatible change can be detected by
// verifiers instead of silently misparsing.
const Version = "1"

// SignatureAlgorithm identifies the (only, for now) signing scheme Sign/Verify use.
const SignatureAlgorithm = "ed25519"

// AgentRef identifies the published agent, mirroring proto/manifests/agent_manifest.schema.json's
// metadata block.
type AgentRef struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Owner   string `json:"owner"`
}

// ModelPolicyRef is the manifest's spec.modelPolicy, unresolved — a capability profile name, never
// a concrete provider/model (docs/adr/0004).
type ModelPolicyRef struct {
	Profile   string   `json:"profile,omitempty"`
	Fallbacks []string `json:"fallbacks,omitempty"`
}

// ToolsRef is the manifest's spec.tools — names only, see the package doc's Scope note.
type ToolsRef struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// Document is the bill of materials itself. Every field is deterministically derived from the
// published manifest's own bytes — no clock, no random ID, so BuildDocument called twice on the
// same manifest bytes always returns an identical Document.
type Document struct {
	ABOMVersion    string         `json:"abom_version"`
	Agent          AgentRef       `json:"agent"`
	ManifestSHA256 string         `json:"manifest_sha256"`
	ModelPolicy    ModelPolicyRef `json:"model_policy"`
	Tools          ToolsRef       `json:"tools"`
	EvalGates      []string       `json:"eval_gates,omitempty"`
}

// BuildDocument extracts a Document from a manifest already parsed into a generic map (as
// yaml.Unmarshal into map[string]any produces) plus the exact raw bytes that were published —
// ManifestSHA256 is a hash of rawManifest, not of any re-serialization, so it verifies against the
// literal file a caller published.
func BuildDocument(manifest map[string]any, rawManifest []byte) (Document, error) {
	metadata, _ := manifest["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	version, _ := metadata["version"].(string)
	owner, _ := metadata["owner"].(string)
	if name == "" || version == "" {
		return Document{}, fmt.Errorf("abom: manifest metadata.name/metadata.version are required")
	}

	spec, _ := manifest["spec"].(map[string]any)

	var modelPolicy ModelPolicyRef
	if mp, ok := spec["modelPolicy"].(map[string]any); ok {
		modelPolicy.Profile, _ = mp["profile"].(string)
		modelPolicy.Fallbacks = stringSlice(mp["fallbacks"])
	}

	var tools ToolsRef
	if t, ok := spec["tools"].(map[string]any); ok {
		tools.Allow = stringSlice(t["allow"])
		tools.Deny = stringSlice(t["deny"])
	}

	sum := sha256.Sum256(rawManifest)

	return Document{
		ABOMVersion:    Version,
		Agent:          AgentRef{Name: name, Version: version, Owner: owner},
		ManifestSHA256: hex.EncodeToString(sum[:]),
		ModelPolicy:    modelPolicy,
		Tools:          tools,
		EvalGates:      stringSlice(spec["evalGates"]),
	}, nil
}

func stringSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// CanonicalBytes is the exact byte sequence Sign/Verify operate over. encoding/json.Marshal on a
// struct always emits fields in the struct's declared order with no extra whitespace, so this is
// already deterministic — a named method mainly documents that Document's JSON encoding IS the
// signed payload, not an implementation detail.
func (d Document) CanonicalBytes() ([]byte, error) {
	return json.Marshal(d)
}

// Signed is a Document plus a real, verifiable Ed25519 signature over its canonical bytes.
type Signed struct {
	Document  Document `json:"document"`
	Algorithm string   `json:"signature_algorithm"`
	Signature string   `json:"signature"`  // base64 standard encoding
	PublicKey string   `json:"public_key"` // base64 standard encoding
}

// Sign signs doc's canonical bytes with priv. Deterministic: the same (doc, priv) pair always
// yields byte-identical Signature and PublicKey fields.
func Sign(doc Document, priv ed25519.PrivateKey) (Signed, error) {
	msg, err := doc.CanonicalBytes()
	if err != nil {
		return Signed{}, fmt.Errorf("abom: encoding document: %w", err)
	}
	sig := ed25519.Sign(priv, msg)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return Signed{}, fmt.Errorf("abom: unexpected public key type")
	}
	return Signed{
		Document:  doc,
		Algorithm: SignatureAlgorithm,
		Signature: base64.StdEncoding.EncodeToString(sig),
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	}, nil
}

// Verify reports whether s.Signature is a valid Ed25519 signature, by s.PublicKey, over
// s.Document's canonical bytes. A Document mutated after signing (any field changed) fails
// verification, since the canonical bytes it re-derives no longer match what was signed.
func Verify(s Signed) (bool, error) {
	if s.Algorithm != SignatureAlgorithm {
		return false, fmt.Errorf("abom: unsupported signature_algorithm %q", s.Algorithm)
	}
	pub, err := base64.StdEncoding.DecodeString(s.PublicKey)
	if err != nil {
		return false, fmt.Errorf("abom: decoding public_key: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(s.Signature)
	if err != nil {
		return false, fmt.Errorf("abom: decoding signature: %w", err)
	}
	msg, err := s.Document.CanonicalBytes()
	if err != nil {
		return false, fmt.Errorf("abom: encoding document: %w", err)
	}
	return ed25519.Verify(ed25519.PublicKey(pub), msg, sig), nil
}

// LoadOrGenerateSigningKey returns the Ed25519 private key to sign with. If hexSeed is a non-empty
// hex-encoded 32-byte Ed25519 seed, that key is used (reproducible across processes/machines — the
// real production path, e.g. AEON_ABOM_SIGNING_KEY). Otherwise a fresh key is generated
// (ephemeral=true): every ABOM signed with it is still genuinely self-verifiable, but a second
// "aeon publish" of the identical manifest will not reproduce the same signature, since the key
// itself differs run to run — callers MUST surface this to the operator (see cmd/aeon's runPublish).
func LoadOrGenerateSigningKey(hexSeed string) (priv ed25519.PrivateKey, ephemeral bool, err error) {
	if hexSeed == "" {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, false, fmt.Errorf("abom: generating an ephemeral signing key: %w", err)
		}
		return priv, true, nil
	}

	seed, err := hex.DecodeString(hexSeed)
	if err != nil {
		return nil, false, fmt.Errorf("abom: AEON_ABOM_SIGNING_KEY is not valid hex: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, false, fmt.Errorf("abom: AEON_ABOM_SIGNING_KEY must decode to %d bytes (an Ed25519 seed), got %d", ed25519.SeedSize, len(seed))
	}
	return ed25519.NewKeyFromSeed(seed), false, nil
}
