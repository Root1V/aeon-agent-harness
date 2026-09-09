package modelgateway

import (
	"context"
	"errors"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/providers"
)

// streamingFakeProvider implements the optional providers.StreamingProvider on top of fakeProvider,
// so the routing tests can tell a streaming-capable adapter from a plain one.
type streamingFakeProvider struct {
	fakeProvider
	chunks    []string
	failAfter int // -1 never fails; otherwise fail after yielding this many chunks
}

func (s *streamingFakeProvider) DecideStream(ctx context.Context, renderedContext map[string]any, yield func(providers.Chunk) error) error {
	for i, c := range s.chunks {
		if s.failAfter >= 0 && i >= s.failAfter {
			return errors.New("simulated mid-stream provider failure")
		}
		if err := yield(providers.Chunk{Delta: c, Model: renderedContext["model"].(string)}); err != nil {
			return err
		}
	}
	return nil
}

func collect(chunks *[]string) func(providers.Chunk) error {
	return func(c providers.Chunk) error {
		*chunks = append(*chunks, c.Delta)
		return nil
	}
}

func TestGatewayDecideStream(t *testing.T) {
	t.Run("streams from a streaming-capable provider", func(t *testing.T) {
		gw := New()
		gw.RegisterProvider("streamer", &streamingFakeProvider{chunks: []string{"a", "b", "c"}, failAfter: -1})

		var got []string
		result, err := gw.DecideStream(context.Background(), []Candidate{
			{Provider: "streamer", Model: "m1", Priority: 0},
		}, map[string]any{}, "", collect(&got))

		if err != nil {
			t.Fatalf("DecideStream: %v", err)
		}
		if !result.Streamed {
			t.Error("Streamed = false, want true for a StreamingProvider")
		}
		if len(got) != 3 || got[0] != "a" || got[2] != "c" {
			t.Fatalf("chunks = %v, want a/b/c in order", got)
		}
	})

	t.Run("a provider without streaming support is served as one chunk", func(t *testing.T) {
		gw := New()
		gw.RegisterProvider("plain", &fakeProvider{responded: map[string]any{}})

		var got []string
		result, err := gw.DecideStream(context.Background(), []Candidate{
			{Provider: "plain", Model: "m1", Priority: 0},
		}, map[string]any{}, "", collect(&got))

		if err != nil {
			t.Fatalf("DecideStream: %v", err)
		}
		if result.Streamed {
			t.Error("Streamed = true, want false — this provider cannot be cancelled mid-generation and the caller must be able to tell")
		}
		if len(got) != 1 {
			t.Fatalf("expected exactly one chunk from a non-streaming provider, got %v", got)
		}
	})

	t.Run("falls back to the next candidate when the first fails before any chunk", func(t *testing.T) {
		gw := New()
		gw.RegisterProvider("broken", &streamingFakeProvider{chunks: []string{"x"}, failAfter: 0})
		gw.RegisterProvider("healthy", &streamingFakeProvider{chunks: []string{"ok"}, failAfter: -1})

		var got []string
		result, err := gw.DecideStream(context.Background(), []Candidate{
			{Provider: "broken", Model: "m1", Priority: 0},
			{Provider: "healthy", Model: "m2", Priority: 1},
		}, map[string]any{}, "", collect(&got))

		if err != nil {
			t.Fatalf("DecideStream: %v", err)
		}
		if result.ProviderUsed != "healthy" {
			t.Fatalf("ProviderUsed = %q, want healthy", result.ProviderUsed)
		}
		if len(got) != 1 || got[0] != "ok" {
			t.Fatalf("chunks = %v, want only the healthy provider's output", got)
		}
	})

	t.Run("does NOT fall back once a chunk has already been delivered", func(t *testing.T) {
		gw := New()
		gw.RegisterProvider("half-broken", &streamingFakeProvider{chunks: []string{"partial", "never"}, failAfter: 1})
		gw.RegisterProvider("healthy", &streamingFakeProvider{chunks: []string{"ok"}, failAfter: -1})

		var got []string
		result, err := gw.DecideStream(context.Background(), []Candidate{
			{Provider: "half-broken", Model: "m1", Priority: 0},
			{Provider: "healthy", Model: "m2", Priority: 1},
		}, map[string]any{}, "", collect(&got))

		if err == nil {
			t.Fatal("expected the mid-stream failure to surface, not be papered over by a fallback")
		}
		if result == nil || result.ProviderUsed != "half-broken" {
			t.Fatalf("expected the failure attributed to the provider that was mid-stream, got %+v", result)
		}
		// The caller must not receive two providers' output spliced into one response.
		if len(got) != 1 || got[0] != "partial" {
			t.Fatalf("chunks = %v, want only what the first provider managed to send", got)
		}
	})

	t.Run("yield's error stops the stream and is returned", func(t *testing.T) {
		gw := New()
		gw.RegisterProvider("streamer", &streamingFakeProvider{chunks: []string{"a", "b", "c"}, failAfter: -1})

		stop := errors.New("budget exhausted")
		delivered := 0
		_, err := gw.DecideStream(context.Background(), []Candidate{
			{Provider: "streamer", Model: "m1", Priority: 0},
		}, map[string]any{}, "", func(providers.Chunk) error {
			delivered++
			return stop
		})

		if !errors.Is(err, stop) {
			t.Fatalf("err = %v, want the caller's own stop error", err)
		}
		if delivered != 1 {
			t.Fatalf("provider kept generating after yield said stop: %d chunks delivered", delivered)
		}
	})

	t.Run("restricted data still never reaches a cloud candidate", func(t *testing.T) {
		gw := New()
		gw.RegisterProvider("anthropic", &streamingFakeProvider{chunks: []string{"leak"}, failAfter: -1})

		var got []string
		_, err := gw.DecideStream(context.Background(), []Candidate{
			{Provider: "anthropic", Model: "m1", Priority: 0},
		}, map[string]any{}, "restricted", collect(&got))

		if !errors.Is(err, ErrNoRestrictedCandidate) {
			t.Fatalf("err = %v, want ErrNoRestrictedCandidate", err)
		}
		if len(got) != 0 {
			t.Fatalf("restricted data reached a cloud provider: %v", got)
		}
	})

	t.Run("a degraded candidate is skipped in streaming too (MDL-002)", func(t *testing.T) {
		gw := New()
		gw.RegisterProvider("degraded", &streamingFakeProvider{chunks: []string{"nope"}, failAfter: -1})
		gw.RegisterProvider("healthy", &streamingFakeProvider{chunks: []string{"ok"}, failAfter: -1})
		gw.Quality = &fakeQualityGate{degraded: map[string]bool{"degraded/m1": true}}

		var got []string
		result, err := gw.DecideStream(context.Background(), []Candidate{
			{Provider: "degraded", Model: "m1", Priority: 0},
			{Provider: "healthy", Model: "m2", Priority: 1},
		}, map[string]any{}, "", collect(&got))

		if err != nil {
			t.Fatalf("DecideStream: %v", err)
		}
		if result.ProviderUsed != "healthy" {
			t.Fatalf("ProviderUsed = %q, want healthy — a degraded candidate must be skipped when streaming too", result.ProviderUsed)
		}
	})
}
