package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RagStore is TOOL-006's retrieval store: real passages from real documents, in pgvector.
type RagStore struct {
	pool *pgxpool.Pool
}

// RagStore returns the TOOL-006 handle.
func (s *Store) RagStore() *RagStore {
	return &RagStore{pool: s.pool}
}

// Chunk is one indexable piece of a document, carrying the locator that makes it citable.
type Chunk struct {
	SourcePath string
	ChunkIndex int
	ByteStart  int
	ByteEnd    int
	Content    string
}

// Locator is what a citation points at. It is the reason chunks carry byte offsets at all: a
// passage without one can be quoted but not checked, and DR-005's Citation Verifier has nothing to
// verify against.
func (c Chunk) Locator() string {
	return fmt.Sprintf("%s#%d-%d", c.SourcePath, c.ByteStart, c.ByteEnd)
}

// Passage is a retrieved Chunk with its similarity to the query.
type Passage struct {
	Chunk
	Locator string  `json:"locator"`
	Score   float64 `json:"score"`
}

// ErrEmbeddingModelMismatch is returned when a corpus is searched with a different embedding model
// than the one that indexed it.
//
// This is a hard error rather than a warning because the failure is invisible otherwise: two
// different embedding spaces compare perfectly well against each other and return confident,
// plausible, wrong passages. Nothing downstream — not the ranking, not the agent, not a human
// reading the answer — can tell the difference.
var ErrEmbeddingModelMismatch = errors.New("store: this corpus was indexed with a different embedding model")

// ErrCorpusEmpty is returned when searching a corpus that has nothing indexed. Distinguishing it
// from "no good matches" matters: one is a missing index, the other is an answer.
var ErrCorpusEmpty = errors.New("store: nothing has been indexed in this corpus")

// Index writes chunks and their embeddings. Re-indexing the same locator replaces it, so running an
// indexer twice over an unchanged corpus converges instead of duplicating passages.
func (r *RagStore) Index(ctx context.Context, corpus, embeddingModel string, chunks []Chunk, embeddings [][]float32) error {
	if len(chunks) != len(embeddings) {
		return fmt.Errorf("store: %d chunks but %d embeddings", len(chunks), len(embeddings))
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin rag index: %w", err)
	}
	defer tx.Rollback(context.Background())

	for i, chunk := range chunks {
		if _, err := tx.Exec(ctx,
			`INSERT INTO rag_chunks (corpus, source_path, chunk_index, byte_start, byte_end, content, embedding_model, embedding)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8::vector)
			 ON CONFLICT (corpus, source_path, chunk_index) DO UPDATE
			   SET byte_start = EXCLUDED.byte_start, byte_end = EXCLUDED.byte_end,
			       content = EXCLUDED.content, embedding_model = EXCLUDED.embedding_model,
			       embedding = EXCLUDED.embedding, indexed_at = now()`,
			corpus, chunk.SourcePath, chunk.ChunkIndex, chunk.ByteStart, chunk.ByteEnd, chunk.Content,
			embeddingModel, vectorLiteral(embeddings[i]),
		); err != nil {
			return fmt.Errorf("store: indexing chunk %d of %s: %w", chunk.ChunkIndex, chunk.SourcePath, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit rag index: %w", err)
	}
	return nil
}

// Search returns the topK nearest passages by cosine distance, refusing outright if the corpus was
// built with a different embedding model.
func (r *RagStore) Search(ctx context.Context, corpus, embeddingModel string, query []float32, topK int) ([]Passage, error) {
	if topK <= 0 {
		topK = 5
	}

	var indexedModel string
	err := r.pool.QueryRow(ctx,
		`SELECT embedding_model FROM rag_chunks WHERE corpus = $1 LIMIT 1`, corpus,
	).Scan(&indexedModel)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil, fmt.Errorf("%w: %q", ErrCorpusEmpty, corpus)
		}
		return nil, fmt.Errorf("store: reading corpus %q: %w", corpus, err)
	}
	if indexedModel != embeddingModel {
		return nil, fmt.Errorf("%w: indexed with %q, searched with %q", ErrEmbeddingModelMismatch, indexedModel, embeddingModel)
	}

	rows, err := r.pool.Query(ctx,
		`SELECT source_path, chunk_index, byte_start, byte_end, content, 1 - (embedding <=> $2::vector)
		   FROM rag_chunks WHERE corpus = $1
		  ORDER BY embedding <=> $2::vector
		  LIMIT $3`,
		corpus, vectorLiteral(query), topK,
	)
	if err != nil {
		return nil, fmt.Errorf("store: searching corpus %q: %w", corpus, err)
	}
	defer rows.Close()

	var out []Passage
	for rows.Next() {
		var p Passage
		if err := rows.Scan(&p.SourcePath, &p.ChunkIndex, &p.ByteStart, &p.ByteEnd, &p.Content, &p.Score); err != nil {
			return nil, fmt.Errorf("store: scanning passage: %w", err)
		}
		p.Locator = p.Chunk.Locator()
		out = append(out, p)
	}
	return out, rows.Err()
}

// vectorLiteral renders an embedding the way pgvector parses it. Building the literal here rather
// than pulling in a driver extension keeps this package's dependency list unchanged for one syntax.
func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
