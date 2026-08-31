// Package a2a is the A2A Gateway (A2A-001): Aeon agents publish a real Agent Card and exchange
// real tasks with any A2A-speaking caller, built on the real, official
// github.com/a2aproject/a2a-go SDK — the same "use the real SDK" convention this codebase already
// follows for MCP (go/internal/mcp), Temporal, OTel and pgx, rather than reimplementing a wire
// protocol.
package a2a

import (
	"fmt"

	sdka2a "github.com/a2aproject/a2a-go/a2a"

	"github.com/aeon-ai/aeon/go/internal/store"
)

// BuildAgentCard maps a real, registered AgentManifest (FND-001, go/internal/store.AgentRecord)
// into a real A2A AgentCard — the identity any A2A caller discovers before ever sending a task, so
// it must reflect the actual governed agent, never a hand-maintained duplicate of its metadata.
// Skills are derived from the manifest's own tools.allow list: what an Aeon agent can invoke is
// exactly what its Cedar policy already governs, not a separately-maintained capability list that
// could drift from it.
func BuildAgentCard(rec *store.AgentRecord, url string) *sdka2a.AgentCard {
	skills := make([]sdka2a.AgentSkill, 0)
	if spec, ok := rec.Manifest["spec"].(map[string]any); ok {
		if tools, ok := spec["tools"].(map[string]any); ok {
			if allow, ok := tools["allow"].([]any); ok {
				for _, t := range allow {
					name, _ := t.(string)
					if name == "" {
						continue
					}
					skills = append(skills, sdka2a.AgentSkill{
						ID:          name,
						Name:        name,
						Description: fmt.Sprintf("Aeon tool %q, governed by this agent's own policy bundle", name),
						Tags:        []string{"aeon-tool"},
					})
				}
			}
		}
	}

	return &sdka2a.AgentCard{
		Name:               rec.Name,
		Description:        fmt.Sprintf("Aeon agent %s@%s", rec.Name, rec.Version),
		URL:                url,
		Version:            rec.Version,
		PreferredTransport: sdka2a.TransportProtocolJSONRPC,
		DefaultInputModes:  []string{"text"},
		DefaultOutputModes: []string{"text"},
		Capabilities:       sdka2a.AgentCapabilities{Streaming: false},
		Skills:             skills,
	}
}
