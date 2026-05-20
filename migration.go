package searchable_history

import "context"

const migrationSQL = `
CREATE TABLE IF NOT EXISTS session_digests (
    session_id  UUID PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    source      TEXT,
    digest      TEXT NOT NULL,
    digest_tsv  TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', digest)) STORED,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS session_digests_tsv_idx ON session_digests USING GIN (digest_tsv);
`

func (e *Extension) migrate(ctx context.Context) error {
	_, err := e.db.Exec(ctx, migrationSQL)
	return err
}
