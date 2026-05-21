package searchable_history

import (
	"context"
	"fmt"
)

// baseMigration creates the session_digests table with full-text search.
// This runs always — it only requires plain PostgreSQL.
const baseMigration = `
CREATE TABLE IF NOT EXISTS session_digests (
    session_id  UUID PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    source      TEXT,
    digest      TEXT NOT NULL,
    digest_tsv  TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', digest)) STORED,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS session_digests_tsv_idx ON session_digests USING GIN (digest_tsv);
`

// vectorMigration adds the embedding column and vector index.
// Only runs when embedding_model is configured.
// Requires the pgvector extension to be installed in PostgreSQL.
const vectorMigration = `
-- Ensure pgvector extension is available.
CREATE EXTENSION IF NOT EXISTS vector;

-- Add embedding column if it doesn't exist.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'session_digests' AND column_name = 'embedding'
    ) THEN
        ALTER TABLE session_digests ADD COLUMN embedding vector(%d);
    END IF;
END $$;

-- Create HNSW index for fast vector similarity search.
CREATE INDEX IF NOT EXISTS session_digests_embedding_idx
    ON session_digests USING hnsw (embedding vector_cosine_ops);
`

// backfillNote is logged when the extension starts with embedding enabled
// but existing digests don't have embeddings yet.
const backfillNote = `
searchable_history: Embedding is enabled but existing digests may not have vectors yet.
  To backfill, run:  beluga extend verify ./internal/extensions/searchable_history
  Or delete and re-process digests:
    DELETE FROM session_digests;
  The digest loop will rebuild them with embeddings on the next tick.
`

// migrate runs the appropriate migrations based on config.
func (e *Extension) migrate(ctx context.Context) error {
	// Always run the base FTS migration.
	if _, err := e.db.Exec(ctx, baseMigration); err != nil {
		return fmt.Errorf("base migration: %w", err)
	}

	// If embedding is configured, run the vector migration.
	if e.cfg.EmbeddingEnabled() {
		vectorSQL := fmt.Sprintf(vectorMigration, e.cfg.EmbeddingDimensions)
		if _, err := e.db.Exec(ctx, vectorSQL); err != nil {
			return fmt.Errorf("vector migration (is pgvector installed?): %w", err)
		}

		// Check if there are digests without embeddings and log a note.
		var missing int
		err := e.db.QueryRow(ctx, `
			SELECT COUNT(*) FROM session_digests WHERE embedding IS NULL
		`).Scan(&missing)
		if err == nil && missing > 0 {
			e.logger.Warn("digests without embeddings detected",
				"missing", missing,
				"hint", "delete from session_digests to rebuild with embeddings, or wait for backfill",
			)
		}
	}

	return nil
}
