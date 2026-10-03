// FND-002: `aeon publish` generates and signs a reproducible Agent Bill of Materials (ABOM) —
// see go/internal/abom for the actual document shape, signing, and verification logic. This file
// is just the CLI-side glue: where the signing key comes from and where the file gets written.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aeon-ai/aeon/go/internal/abom"
	"github.com/aeon-ai/aeon/go/internal/secretref"
)

// writeABOM builds and signs the ABOM for a just-published manifest and writes it next to the
// manifest file as <name>-<version>.abom.json. Returns the written path, the signer's public key
// (base64), and whether the signing key was ephemeral (generated fresh for this one invocation,
// because AEON_ABOM_SIGNING_KEY wasn't set) — callers should surface that to the operator, since an
// ephemeral key means this exact signature will never reproduce on a later run.
func writeABOM(manifestPath string, manifest map[string]any, rawManifest []byte) (path string, publicKey string, ephemeral bool, err error) {
	doc, err := abom.BuildDocument(manifest, rawManifest)
	if err != nil {
		return "", "", false, err
	}

	// SEC-006: AEON_ABOM_SIGNING_KEY_FILE also works, and the seed leaves this process's
	// environment once read. A leaked seed means anyone can sign an ABOM that verifies, which makes
	// FND-002's whole chain decorative.
	signingKey, _, err := secretref.Take("AEON_ABOM_SIGNING_KEY")
	if err != nil {
		return "", "", false, err
	}
	priv, ephemeral, err := abom.LoadOrGenerateSigningKey(signingKey)
	if err != nil {
		return "", "", false, err
	}

	signed, err := abom.Sign(doc, priv)
	if err != nil {
		return "", "", false, err
	}

	encoded, err := json.MarshalIndent(signed, "", "  ")
	if err != nil {
		return "", "", false, fmt.Errorf("encoding signed ABOM: %w", err)
	}

	abomPath := filepath.Join(filepath.Dir(manifestPath), fmt.Sprintf("%s-%s.abom.json", doc.Agent.Name, doc.Agent.Version))
	if err := os.WriteFile(abomPath, encoded, 0o644); err != nil {
		return "", "", false, fmt.Errorf("writing %s: %w", abomPath, err)
	}

	return abomPath, signed.PublicKey, ephemeral, nil
}
