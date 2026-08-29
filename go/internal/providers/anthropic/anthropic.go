// Package anthropic is the anthropic adapter behind the Provider interface (docs/adr/0004). It is a
// stub: the real HTTP client, structured-output handling, and tool-calling translation are not
// yet implemented — see roadmap.md MDL-003..007.
package anthropic

import "github.com/aeon-ai/aeon/go/internal/providers"

// Name identifies this adapter in ModelPolicyBundle candidates (proto/schemas/model_profile.schema.json).
const Name = "anthropic"

// Provider is an alias for the shared interface (go/internal/providers) every model adapter
// implements — kept here so existing references to anthropic.Provider keep working.
type Provider = providers.Provider
