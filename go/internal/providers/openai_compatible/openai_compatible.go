// Package openaicompatible is the openai_compatible adapter behind the Provider interface (docs/adr/0004). It is a
// stub: the real HTTP client, structured-output handling, and tool-calling translation are not
// yet implemented — see roadmap.md MDL-003..007.
package openaicompatible

import "github.com/aeon-ai/aeon/go/internal/providers"

// Name identifies this adapter in ModelPolicyBundle candidates (proto/schemas/model_profile.schema.json).
const Name = "openai_compatible"

// Provider is an alias for the shared interface (go/internal/providers) every model adapter
// implements — kept here so existing references to openaicompatible.Provider keep working.
type Provider = providers.Provider
