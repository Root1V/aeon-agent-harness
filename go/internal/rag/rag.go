// Package rag is TOOL-006: retrieval over real documents, behind an interface that names no
// provider.
//
// The Embedder is an interface for the reason MDL-017 made explicit: a harness that knows one
// platform's name by heart is coupled to it. The deployment supplies an embedder; this package only
// knows that text goes in and vectors come out.
package rag

import (
	"context"
	"fmt"
	"strings"

	"github.com/aeon-ai/aeon/go/internal/store"
)

// Embedder turns text into vectors. Implementations live with the provider that knows how.
type Embedder interface {
	// Embed returns one vector per input, in the same order. Model reports which model produced
	// them, which the store records per chunk so a later search with a different one can be refused
	// rather than answered wrongly.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
}

// maxChunkBytes bounds a chunk so a passage stays quotable and its embedding stays meaningful.
// Embedding a whole document produces one vector that is the average of everything it says, which
// retrieves nothing well.
const maxChunkBytes = 1200

// ChunkDocument splits a document on blank lines, packing paragraphs up to maxChunkBytes, and
// records the byte range each chunk came from.
//
// Byte offsets are the point. A chunk without them can be shown to a reader but not located in the
// source, and a citation that cannot be located is an assertion — which is exactly what DR-005's
// Citation Verifier exists to reject.
func ChunkDocument(sourcePath, content string) []store.Chunk {
	var chunks []store.Chunk
	var start int
	var buf strings.Builder

	flush := func(end int) {
		text := strings.TrimSpace(buf.String())
		if text == "" {
			buf.Reset()
			return
		}
		chunks = append(chunks, store.Chunk{
			SourcePath: sourcePath,
			ChunkIndex: len(chunks),
			ByteStart:  start,
			ByteEnd:    end,
			Content:    text,
		})
		buf.Reset()
	}

	offset := 0
	for _, paragraph := range strings.SplitAfter(content, "\n\n") {
		if buf.Len() > 0 && buf.Len()+len(paragraph) > maxChunkBytes {
			flush(offset)
			start = offset
		}
		if buf.Len() == 0 {
			start = offset
		}
		buf.WriteString(paragraph)
		offset += len(paragraph)
	}
	flush(offset)
	return chunks
}

// IndexDocument chunks, embeds and stores one document.
func IndexDocument(ctx context.Context, s *store.RagStore, e Embedder, corpus, sourcePath, content string) (int, error) {
	chunks := ChunkDocument(sourcePath, content)
	if len(chunks) == 0 {
		return 0, nil
	}
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Content
	}
	vectors, err := e.Embed(ctx, texts)
	if err != nil {
		return 0, fmt.Errorf("rag: embedding %s: %w", sourcePath, err)
	}
	if err := s.Index(ctx, corpus, e.Model(), chunks, vectors); err != nil {
		return 0, err
	}
	return len(chunks), nil
}

// Search embeds the query and returns the nearest passages, each with its locator.
func Search(ctx context.Context, s *store.RagStore, e Embedder, corpus, query string, topK int) ([]store.Passage, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("rag: query is required")
	}
	vectors, err := e.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("rag: embedding query: %w", err)
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("rag: embedder returned %d vectors for one query", len(vectors))
	}
	return s.Search(ctx, corpus, e.Model(), vectors[0], topK)
}
