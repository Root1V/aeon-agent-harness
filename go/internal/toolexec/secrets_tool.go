package toolexec

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/aeon-ai/aeon/go/internal/secrets"
)

// RegisterSecretsTool wires "secrets.whoami" (SEC-002) into e, resolving leases against broker.
// Not part of NewExecutor()'s default set — a caller opts in by calling this explicitly (see
// go/cmd/aeon-toolgw/main.go), the same way any other real tool implementation gets added.
//
// "secrets.whoami" exists to demonstrate — and let go/internal/api's TestNoSecretInPrompt prove —
// the whole point of a Secret Broker: the caller only ever holds an opaque, short-lived lease
// reference (secret_ref); the raw value is resolved here, server-side, at the moment of actual use,
// and never appears in this tool's own result. A SHA-256 fingerprint proves the resolution really
// happened (a different secret produces a different fingerprint) without being reversible to the
// raw value.
func RegisterSecretsTool(e *Executor, broker *secrets.Broker) {
	e.Register("secrets.whoami", func(args map[string]any) (map[string]any, error) {
		ref, ok := args["secret_ref"].(string)
		if !ok || ref == "" {
			return nil, fmt.Errorf("toolexec: secrets.whoami: missing required string arg %q", "secret_ref")
		}
		value, err := broker.Resolve(ref)
		if err != nil {
			return nil, fmt.Errorf("toolexec: secrets.whoami: %w", err)
		}
		sum := sha256.Sum256([]byte(value))
		return map[string]any{
			"status":             "executed",
			"tool":               "secrets.whoami",
			"secret_fingerprint": hex.EncodeToString(sum[:8]),
		}, nil
	})
}
