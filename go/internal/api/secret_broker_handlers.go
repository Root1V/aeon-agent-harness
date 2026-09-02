package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/aeon-ai/aeon/go/internal/secrets"
)

// SecretBrokerHandlers exposes SEC-002's lease-issuance surface: a caller preparing to invoke a
// tool that needs a credential gets back an opaque, short-lived reference — never the secret value
// itself. Only a tool's own server-side execution ever resolves a lease to its real value (see
// toolexec.RegisterSecretsTool) — this handler deliberately has no "resolve"/"reveal" route at all.
type SecretBrokerHandlers struct {
	Broker *secrets.Broker
}

// Register mounts the secret broker routes on mux.
func (h *SecretBrokerHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /secrets/issue", h.issue)
}

type issueLeaseRequest struct {
	Name       string `json:"name"`
	TTLSeconds int    `json:"ttl_seconds"`
}

func (h *SecretBrokerHandlers) issue(w http.ResponseWriter, r *http.Request) {
	var body issueLeaseRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	var ttl time.Duration
	if body.TTLSeconds > 0 {
		ttl = time.Duration(body.TTLSeconds) * time.Second
	}

	ref, expiresAt, err := h.Broker.Issue(body.Name, ttl)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ref":        ref,
		"expires_at": expiresAt,
	})
}
