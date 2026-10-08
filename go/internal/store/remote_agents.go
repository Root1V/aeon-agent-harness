package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrRemoteAgentNotDeclared is returned when nothing has declared the destination of a delegation.
//
// A distinct error rather than a generic not-found, because the egress proxy has to tell two refusals
// apart when it explains itself: a destination nobody declared is a missing registry entry, and a
// declared destination Cedar refused is a policy decision. They look alike from the caller's side and
// they are fixed in completely different places.
var ErrRemoteAgentNotDeclared = errors.New("store: remote agent is not declared in the registry")

// RemoteAgentRecord is a declared delegation destination (A2A-002).
type RemoteAgentRecord struct {
	AgentID     string `json:"agent_id"`
	URL         string `json:"url"`
	Risk        string `json:"risk"`
	Description string `json:"description,omitempty"`
	// CredentialSecretName names a secret the Secret Broker holds. The VALUE is never stored here and
	// never reaches the governed process — the egress proxy resolves it and injects it on the way out.
	// Empty means the destination needs no credential, which is a declaration; it is not the same as
	// "we have no credential for it", which would be an omission.
	CredentialSecretName string    `json:"credential_secret_name,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// RemoteAgents is the Postgres-backed registry of delegation destinations.
type RemoteAgents struct {
	pool   *pgxpool.Pool
	tenant string
}

// RemoteAgents returns a handle for A2A-002's remote agent registry.
// RemoteAgentsFor returns the handle SCOPED TO ONE TENANT (VRT-AEON-005 T-4).
//
// The tenant is a CONSTRUCTOR argument, not a method parameter, and that is the seam: slice 2 found
// the Memory Store's isolation implemented route by route — four routes read the tenant from the
// request and five checked none at all — because every method was a separate chance to forget. A
// handle that cannot exist without a tenant makes the compiler answer that once, here.
func (s *Store) RemoteAgentsFor(tenant string) *RemoteAgents {
	return &RemoteAgents{pool: s.pool, tenant: tenant}
}

// Declare registers a delegation destination, or updates one already declared.
//
// risk is REQUIRED and is validated here rather than defaulted. Synaptum defaults theirs to the most
// dangerous class, which is also fail-safe; the reason this asks instead is that a default is silent —
// nobody ever learns the declaration was missing — while a rejected write makes a person look once. On
// the other side of a delegation there is a model deciding, and it can change without telling us.
func (r *RemoteAgents) Declare(ctx context.Context, rec RemoteAgentRecord) (*RemoteAgentRecord, error) {
	if rec.AgentID == "" {
		return nil, fmt.Errorf("store: remote agent needs an agent_id")
	}
	if rec.URL == "" {
		return nil, fmt.Errorf("store: remote agent %q needs a url", rec.AgentID)
	}
	switch rec.Risk {
	case RiskLow, RiskMedium, RiskHigh, RiskCritical:
	default:
		return nil, fmt.Errorf("store: remote agent %q declares risk %q, want one of %s/%s/%s/%s — a delegation destination with no declared risk is not registered, the same rule tools obey",
			rec.AgentID, rec.Risk, RiskLow, RiskMedium, RiskHigh, RiskCritical)
	}

	var out RemoteAgentRecord
	err := r.pool.QueryRow(ctx,
		`INSERT INTO remote_agents (tenant_id, agent_id, url, risk, description, credential_secret_name)
		 VALUES ($6, $1, $2, $3, NULLIF($4, ''), NULLIF($5, ''))
		 ON CONFLICT (tenant_id, agent_id) DO UPDATE SET
		   url = EXCLUDED.url, risk = EXCLUDED.risk, description = EXCLUDED.description,
		   credential_secret_name = EXCLUDED.credential_secret_name, updated_at = now()
		 RETURNING agent_id, url, risk, COALESCE(description, ''), COALESCE(credential_secret_name, ''), created_at, updated_at`,
		rec.AgentID, rec.URL, rec.Risk, rec.Description, rec.CredentialSecretName, r.tenant,
	).Scan(&out.AgentID, &out.URL, &out.Risk, &out.Description, &out.CredentialSecretName, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("store: declaring remote agent %q: %w", rec.AgentID, err)
	}
	return &out, nil
}

// Get returns a declared destination, or ErrRemoteAgentNotDeclared.
func (r *RemoteAgents) Get(ctx context.Context, agentID string) (*RemoteAgentRecord, error) {
	var out RemoteAgentRecord
	err := r.pool.QueryRow(ctx,
		`SELECT agent_id, url, risk, COALESCE(description, ''), COALESCE(credential_secret_name, ''), created_at, updated_at
		   FROM remote_agents WHERE tenant_id = $2 AND agent_id = $1`, agentID, r.tenant,
	).Scan(&out.AgentID, &out.URL, &out.Risk, &out.Description, &out.CredentialSecretName, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrRemoteAgentNotDeclared, agentID)
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading remote agent %q: %w", agentID, err)
	}
	return &out, nil
}

// List returns every declared destination, newest declaration last.
func (r *RemoteAgents) List(ctx context.Context) ([]RemoteAgentRecord, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT agent_id, url, risk, COALESCE(description, ''), COALESCE(credential_secret_name, ''), created_at, updated_at
		   FROM remote_agents WHERE tenant_id = $1 ORDER BY created_at, agent_id`, r.tenant)
	if err != nil {
		return nil, fmt.Errorf("store: listing remote agents: %w", err)
	}
	defer rows.Close()

	out := []RemoteAgentRecord{}
	for rows.Next() {
		var rec RemoteAgentRecord
		if err := rows.Scan(&rec.AgentID, &rec.URL, &rec.Risk, &rec.Description,
			&rec.CredentialSecretName, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scanning remote agent: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
