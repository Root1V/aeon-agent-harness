package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	prometheusinference "github.com/aeon-ai/aeon/go/internal/providers/prometheus_inference"
	"github.com/aeon-ai/aeon/go/internal/rag"
	"github.com/aeon-ai/aeon/go/internal/secretref"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// indexableExtensions is deliberately narrow. Indexing a binary produces chunks of mojibake that
// embed to nothing meaningful and then surface as confident passages — worse than not indexing it,
// because the agent cites them.
var indexableExtensions = map[string]bool{
	".md": true, ".txt": true, ".rst": true, ".adoc": true,
}

// runRagIndex is TOOL-006's way in: walk a directory, chunk what it finds, embed it and store it
// with the locator that makes each passage citable.
func runRagIndex(dir, corpus string) error {
	dsn := os.Getenv("AEON_PG_DSN")
	if dsn == "" {
		return fmt.Errorf("AEON_PG_DSN is required (the corpus lives in Postgres)")
	}
	model := os.Getenv("AEON_EMBEDDING_MODEL")
	if model == "" {
		return fmt.Errorf("AEON_EMBEDDING_MODEL is required — and it must be a model your token carries a model:<id> scope for, not merely one you are authorised for")
	}

	ctx := context.Background()
	s, err := store.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connecting to Postgres: %w", err)
	}
	defer s.Close()

	// SEC-006: the credential leaves this process's environment once read, which matters in the CLI
	// specifically because `aeon eval run` and `aeon replay` exec a Python interpreter with no
	// cmd.Env — so before this, every provider secret in the operator's shell was handed to a child
	// process that has no use for it.
	clientSecret, _, err := secretref.Take("PROMETHEUS_CLIENT_SECRET")
	if err != nil {
		return err
	}

	embedder := &prometheusinference.Embedder{
		Client: &prometheusinference.Client{
			// MDL-009: one address. PROMETHEUS_AUTH_URL is no longer read — the gateway serves
			// /oauth2/token too, and keeping a second address was the footgun Axonium removed from
			// their own SDK: two values to change, and forgetting one minted tokens against the wrong
			// party with nothing failing.
			GatewayURL:   os.Getenv("PROMETHEUS_GATEWAY_URL"),
			ClientID:     os.Getenv("PROMETHEUS_CLIENT_ID"),
			ClientSecret: clientSecret,
			Scope:        "inference:read model:" + model,
		},
		ModelID: model,
	}

	// VRT-AEON-005: the CLI indexes into the tenant named by AEON_TENANT_ID, defaulting to the same
	// "default" the migration backfilled and the shipped caller bundles declare. The CLI is a
	// person's tool and has no caller bundle, so the operator's environment is the only place the
	// tenant can come from here — stated rather than silently assumed.
	tenant := os.Getenv("AEON_TENANT_ID")
	if tenant == "" {
		tenant = "default"
	}
	ragStore := s.RagStoreFor(tenant)
	var files, chunks int
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !indexableExtensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		// The locator is stored relative to the indexed directory, so a corpus stays addressable
		// after it moves — an absolute path from one machine cites nothing on another.
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			rel = path
		}
		n, err := rag.IndexDocument(ctx, ragStore, embedder, corpus, rel, string(content))
		if err != nil {
			return err
		}
		files++
		chunks += n
		fmt.Printf("  %s -> %d chunk(s)\n", rel, n)
		return nil
	})
	if err != nil {
		return err
	}
	if files == 0 {
		return fmt.Errorf("no indexable files found under %s (looking for %s)", dir, strings.Join(sortedExtensions(), ", "))
	}
	fmt.Printf("indexed %d file(s), %d chunk(s) into corpus %q with %s\n", files, chunks, corpus, model)
	return nil
}

func sortedExtensions() []string {
	out := make([]string, 0, len(indexableExtensions))
	for ext := range indexableExtensions {
		out = append(out, ext)
	}
	return out
}
