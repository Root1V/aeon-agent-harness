package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestRemoteAgentMustBeDeclaredWithRisk covers constraint (c) agreed with Synaptum on 2026-09-20: a
// remote agent is declared, with an explicit risk, before anything can delegate to it.
//
// We diverge from Synaptum here on purpose and this test is where the divergence lives. They default to
// their most dangerous class; we refuse the write. Both are fail-safe, and the reason to ask instead of
// defaulting is that a default is SILENT — nobody ever learns the declaration was missing — while a
// refused write makes a person look once. That is worth more for a remote agent than for a tool: on the
// other side there is a model deciding, and it can change without telling us.
func TestRemoteAgentMustBeDeclaredWithRisk(t *testing.T) {
	ctx := context.Background()
	reg := newTestStore(t).RemoteAgentsFor("default")
	id := fmt.Sprintf("remote-%d", time.Now().UnixNano())

	t.Run("a destination with no declared risk is refused", func(t *testing.T) {
		if _, err := reg.Declare(ctx, RemoteAgentRecord{AgentID: id, URL: "http://example.invalid"}); err == nil {
			t.Fatal("a remote agent with no risk was declared — the declaration would exist while saying nothing about what it authorizes")
		}
	})

	t.Run("an invented risk class is refused", func(t *testing.T) {
		// The closed set matters as much as the requirement. A free-text risk would satisfy "declared" while
		// being unreadable by anything that has to act on it.
		if _, err := reg.Declare(ctx, RemoteAgentRecord{AgentID: id, URL: "http://example.invalid", Risk: "SPICY"}); err == nil {
			t.Fatal("risk SPICY was accepted")
		}
	})

	t.Run("a destination with no url is refused", func(t *testing.T) {
		if _, err := reg.Declare(ctx, RemoteAgentRecord{AgentID: id, Risk: RiskLow}); err == nil {
			t.Fatal("a remote agent with no url was declared — there is nowhere to delegate to")
		}
	})

	t.Run("an undeclared destination reads back as NOT DECLARED, distinctly", func(t *testing.T) {
		// A distinct error rather than a generic not-found, because the egress proxy has to tell two
		// refusals apart when it explains itself: nobody declared this, versus policy refused it. They look
		// the same from outside and are fixed in completely different places by different people.
		_, err := reg.Get(ctx, "nothing-ever-declared-this")
		if !errors.Is(err, ErrRemoteAgentNotDeclared) {
			t.Fatalf("err = %v, want ErrRemoteAgentNotDeclared", err)
		}
	})

	t.Run("a declared destination round-trips, and the credential VALUE is never stored", func(t *testing.T) {
		rec, err := reg.Declare(ctx, RemoteAgentRecord{
			AgentID: id, URL: "http://partner.invalid/a2a", Risk: RiskHigh,
			Description: "acceptance", CredentialSecretName: "a2a.partner.token",
		})
		if err != nil {
			t.Fatalf("Declare: %v", err)
		}
		if rec.Risk != RiskHigh || rec.URL != "http://partner.invalid/a2a" {
			t.Errorf("record = %+v", rec)
		}
		// The registry holds the secret's NAME. The value lives in the Secret Broker and is injected by the
		// proxy per call — a registry that stored credentials would put them one SELECT away from anything
		// that can read the catalogue.
		if rec.CredentialSecretName != "a2a.partner.token" {
			t.Errorf("credential_secret_name = %q", rec.CredentialSecretName)
		}

		got, err := reg.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Risk != RiskHigh {
			t.Errorf("risk read back as %q", got.Risk)
		}
	})

	t.Run("re-declaring updates rather than duplicating", func(t *testing.T) {
		// A destination whose URL or risk changed is the SAME destination. Inserting a second row would make
		// Get ambiguous, and a policy written against the id would authorize both.
		if _, err := reg.Declare(ctx, RemoteAgentRecord{AgentID: id, URL: "http://moved.invalid", Risk: RiskCritical}); err != nil {
			t.Fatalf("re-declare: %v", err)
		}
		got, err := reg.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.URL != "http://moved.invalid" || got.Risk != RiskCritical {
			t.Errorf("record after re-declaration = %+v", got)
		}
		// And the credential name is gone, because the new declaration did not carry one. An update that
		// kept it would leave a destination authorized by a credential nobody declared for it.
		if got.CredentialSecretName != "" {
			t.Errorf("credential_secret_name = %q, want empty after a declaration that omitted it", got.CredentialSecretName)
		}
	})
}
