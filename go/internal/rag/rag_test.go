package rag_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	prometheusinference "github.com/aeon-ai/aeon/go/internal/providers/prometheus_inference"
	"github.com/aeon-ai/aeon/go/internal/rag"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// fixedEmbedder is a deterministic stand-in used only where the property under test needs TWO
// embedding models to exist. It is not used for the end-to-end proof: that one runs against the
// real platform, because a fake embedder would make retrieval quality a property of this file.
type fixedEmbedder struct {
	model string
	// vectors maps a text to the vector it should produce. Anything else gets a zero-ish vector,
	// which keeps ranking trivially predictable.
	vectors map[string][]float32
}

func (f *fixedEmbedder) Model() string { return f.model }

func (f *fixedEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		vec := make([]float32, 1024)
		if v, ok := f.vectors[text]; ok {
			copy(vec, v)
		} else {
			vec[0] = 0.001
		}
		out[i] = vec
	}
	return out, nil
}

func testStore(t *testing.T) *store.RagStore {
	t.Helper()
	dsn := os.Getenv("AEON_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("AEON_TEST_PG_DSN not set — see make test-go-integration")
	}
	s, err := store.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)
	return s.RagStore()
}

func realEmbedder(t *testing.T) *prometheusinference.Embedder {
	t.Helper()
	gateway := os.Getenv("AEON_TEST_PROMETHEUS_GATEWAY_URL")
	authURL := os.Getenv("AEON_TEST_PROMETHEUS_AUTH_URL")
	clientID := os.Getenv("AEON_TEST_PROMETHEUS_CLIENT_ID")
	secret := os.Getenv("AEON_TEST_PROMETHEUS_CLIENT_SECRET")
	model := os.Getenv("AEON_TEST_EMBEDDING_MODEL")
	if gateway == "" || authURL == "" || clientID == "" || secret == "" || model == "" {
		t.Skip("Prometheus embedding credentials not set — skipping the real-platform half of TOOL-006")
	}
	return &prometheusinference.Embedder{
		Client: &prometheusinference.Client{
			GatewayURL: gateway,
			Tokens: &prometheusinference.TokenSource{
				AuthURL: authURL, ClientID: clientID, ClientSecret: secret,
				Scope: "inference:read model:" + model,
			},
		},
		ModelID: model,
	}
}

// The two documents disagree on purpose: retrieval has to pick the one that answers the question,
// not the one that shares more words with it.
const durabilityDoc = `El arnés persiste el diario de un run y nunca decide nada sobre él.

La clave de idempotencia es (run_id, step_id, phase). Un duplicado es un no-op y jamás un error,
porque quien corre bajo ejecución at-least-once no puede distinguir un reintento de un primer
intento.

Un duplicado que llega con un resultado distinto conserva el primero y reporta la divergencia.`

const routingDoc = `El Model Gateway enruta un perfil de capacidad a un proveedor concreto.

Cuando un candidato falla, prueba el siguiente en orden de prioridad. En streaming esa cascada deja
de valer en cuanto ha salido el primer token, porque empalmaría dos generaciones distintas.

Los datos marcados como restringidos solo salen hacia proveedores declarados dentro de la red.`

// TestSearchRagReturnsRealPassagesFromIndexedDocuments is TOOL-006's acceptance test.
//
// Before this, search.rag was a stub that echoed its own arguments, and search.web still is one.
// The agent in examples/deep-research is allowed to call both and neither did anything, so a "deep
// research" run retrieved nothing and reported success.
func TestSearchRagReturnsRealPassagesFromIndexedDocuments(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	t.Run("indexed documents are retrieved by meaning, with a locator", func(t *testing.T) {
		embedder := realEmbedder(t)
		corpus := "tool006-real-" + randomSuffix()

		for path, content := range map[string]string{
			"docs/durabilidad.md": durabilityDoc,
			"docs/enrutado.md":    routingDoc,
		} {
			n, err := rag.IndexDocument(ctx, s, embedder, corpus, path, content)
			if err != nil {
				t.Fatalf("indexing %s: %v", path, err)
			}
			if n == 0 {
				t.Fatalf("indexing %s produced no chunks", path)
			}
		}

		// The query shares almost no vocabulary with the passage that answers it — "qué pasa si una
		// herramienta se ejecuta dos veces" against text about idempotency keys and duplicates. A
		// keyword search would miss it, which is the whole reason for embedding anything.
		passages, err := rag.Search(ctx, s, embedder, corpus, "¿qué ocurre si un paso se ejecuta dos veces?", 3)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(passages) == 0 {
			t.Fatal("no passages returned")
		}

		top := passages[0]
		if top.SourcePath != "docs/durabilidad.md" {
			t.Errorf("top passage came from %s, want docs/durabilidad.md — retrieval picked the wrong document", top.SourcePath)
		}
		if top.Locator == "" || !strings.Contains(top.Locator, "#") {
			t.Errorf("locator = %q, want path#start-end — a passage that cannot be located cannot be cited", top.Locator)
		}
		if top.Content == "" {
			t.Error("the top passage has no content")
		}
		// The locator must actually point at the returned text in the original document.
		if top.ByteEnd <= top.ByteStart || top.ByteEnd > len(durabilityDoc) {
			t.Errorf("byte range %d-%d does not fit the source document (%d bytes)", top.ByteStart, top.ByteEnd, len(durabilityDoc))
		}
		if !strings.Contains(durabilityDoc[top.ByteStart:top.ByteEnd], strings.SplitN(top.Content, "\n", 2)[0]) {
			t.Errorf("the locator's byte range does not contain the passage it labels — the citation would point somewhere else")
		}
		t.Logf("top passage: %s (score %.4f)", top.Locator, top.Score)
	})

	t.Run("re-indexing the same document converges instead of duplicating passages", func(t *testing.T) {
		embedder := &fixedEmbedder{model: "fixed-a"}
		corpus := "tool006-reindex-" + randomSuffix()

		first, err := rag.IndexDocument(ctx, s, embedder, corpus, "docs/a.md", durabilityDoc)
		if err != nil {
			t.Fatalf("first index: %v", err)
		}
		if _, err := rag.IndexDocument(ctx, s, embedder, corpus, "docs/a.md", durabilityDoc); err != nil {
			t.Fatalf("second index: %v", err)
		}

		passages, err := rag.Search(ctx, s, embedder, corpus, "cualquier cosa", 50)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(passages) != first {
			t.Fatalf("after indexing twice the corpus holds %d passages, want %d — an indexer that duplicates makes every answer cite the same text twice", len(passages), first)
		}
	})

	t.Run("searching with a different embedding model is refused, not answered", func(t *testing.T) {
		// This is the failure that has no symptom: two embedding spaces compare perfectly well
		// against each other and return confident, plausible, wrong passages. Nothing downstream can
		// tell. Recording the model per chunk is the only thing that allows a refusal.
		corpus := "tool006-mismatch-" + randomSuffix()
		indexer := &fixedEmbedder{model: "model-a"}
		if _, err := rag.IndexDocument(ctx, s, indexer, corpus, "docs/a.md", durabilityDoc); err != nil {
			t.Fatalf("index: %v", err)
		}

		searcher := &fixedEmbedder{model: "model-b"}
		_, err := rag.Search(ctx, s, searcher, corpus, "algo", 3)
		if err == nil {
			t.Fatal("searching with a different embedding model returned results instead of refusing")
		}
		if !strings.Contains(err.Error(), "different embedding model") {
			t.Errorf("error = %v, want it to name the mismatch", err)
		}
	})

	t.Run("an empty corpus is distinguishable from no good matches", func(t *testing.T) {
		embedder := &fixedEmbedder{model: "fixed-a"}
		_, err := rag.Search(ctx, s, embedder, "tool006-empty-"+randomSuffix(), "algo", 3)
		if err == nil {
			t.Fatal("searching an unindexed corpus returned no error — a missing index and an unhelpful answer are different facts")
		}
		if !strings.Contains(err.Error(), "nothing has been indexed") {
			t.Errorf("error = %v, want it to say the corpus is empty", err)
		}
	})

	t.Run("chunking records byte ranges that address the original text", func(t *testing.T) {
		chunks := rag.ChunkDocument("docs/a.md", durabilityDoc)
		if len(chunks) == 0 {
			t.Fatal("no chunks")
		}
		for _, c := range chunks {
			if c.ByteStart < 0 || c.ByteEnd > len(durabilityDoc) || c.ByteEnd <= c.ByteStart {
				t.Fatalf("chunk %d has an unusable range %d-%d", c.ChunkIndex, c.ByteStart, c.ByteEnd)
			}
			slice := durabilityDoc[c.ByteStart:c.ByteEnd]
			if !strings.Contains(slice, strings.SplitN(c.Content, "\n", 2)[0]) {
				t.Fatalf("chunk %d's range does not contain its own first line", c.ChunkIndex)
			}
		}
	})
}

// randomSuffix keeps each subtest in its own corpus, so they can run in any order and a leftover
// row from an earlier run cannot make a later one pass.
func randomSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}
