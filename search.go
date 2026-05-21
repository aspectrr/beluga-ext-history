package searchable_history

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/collinpfeifer/beluga/pkg/tools"
	"github.com/jackc/pgx/v5/pgxpool"
	"log/slog"
)

// HistorySearchTool searches past session digests.
// Uses full-text search by default. When an embedder is available,
// falls back to vector similarity search.
type HistorySearchTool struct {
	db       *pgxpool.Pool
	embedder Embedder
	cfg      Config
	logger   *slog.Logger
}

func (t *HistorySearchTool) Definition() tools.ToolDef {
	return tools.ToolDef{
		Name:        "history_search",
		Description: "Search past session history for relevant context. Returns matching session digests containing the conversation (user and agent messages only).",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "Search query - keywords or phrases to find relevant past sessions"
				},
				"limit": {
					"type": "integer",
					"description": "Maximum number of results to return (default 5)"
				}
			},
			"required": ["query"]
		}`),
	}
}

// SearchResult represents a single matching session digest.
type SearchResult struct {
	SessionID string    `json:"session_id"`
	Source    string    `json:"source"`
	Digest    string    `json:"digest"`
	CreatedAt time.Time `json:"created_at"`
	Rank      float64   `json:"rank"`
}

func (t *HistorySearchTool) Execute(ctx context.Context, args json.RawMessage, tctx tools.ToolContext) (json.RawMessage, error) {
	// Dry-run mode.
	if os.Getenv("BELUGA_DRY_RUN") == "true" {
		return json.Marshal([]SearchResult{
			{
				SessionID: "00000000-0000-0000-0000-000000000001",
				Source:    "clickup",
				Digest:    "User: How do I fix the kafka consumer lag?\nAgent: I'll investigate the consumer group.\nAgent: Found it — the consumer is hitting OOMKill.\n",
				CreatedAt: time.Now().Add(-24 * time.Hour),
				Rank:      0.5,
			},
		})
	}

	var input struct {
		Query string `json:"query"`
		Limit int    `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, fmt.Errorf("parsing args: %w", err)
	}
	if input.Limit <= 0 {
		input.Limit = 5
	}

	// Use vector search if an embedder is available, otherwise fall back to FTS.
	if t.embedder != nil {
		return t.vectorSearch(ctx, input.Query, input.Limit)
	}
	return t.fullTextSearch(ctx, input.Query, input.Limit)
}

// fullTextSearch searches using PostgreSQL tsvector/tsquery.
func (t *HistorySearchTool) fullTextSearch(ctx context.Context, query string, limit int) (json.RawMessage, error) {
	rows, err := t.db.Query(ctx, `
		SELECT sd.session_id, sd.source, sd.digest, sd.created_at,
		       ts_rank(sd.digest_tsv, plainto_tsquery('english', $1)) as rank
		FROM session_digests sd
		WHERE sd.digest_tsv @@ plainto_tsquery('english', $1)
		ORDER BY rank DESC
		LIMIT $2
	`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("searching history: %w", err)
	}
	defer rows.Close()

	return scanResults(rows)
}

// vectorSearch searches using pgvector cosine similarity.
func (t *HistorySearchTool) vectorSearch(ctx context.Context, query string, limit int) (json.RawMessage, error) {
	embedding, err := t.embedder.Embed(ctx, query)
	if err != nil {
		// Fall back to FTS if embedding fails.
		t.logger.Warn("embedding failed, falling back to full-text search", "error", err)
		return t.fullTextSearch(ctx, query, limit)
	}

	vec := formatVector(embedding)
	rows, err := t.db.Query(ctx, `
		SELECT sd.session_id, sd.source, sd.digest, sd.created_at,
		       1 - (sd.embedding <=> $1::vector) as rank
		FROM session_digests sd
		WHERE sd.embedding IS NOT NULL
		ORDER BY sd.embedding <=> $1::vector
		LIMIT $2
	`, vec, limit)
	if err != nil {
		// Fall back to FTS if vector search fails (e.g. no pgvector).
		t.logger.Warn("vector search failed, falling back to full-text search", "error", err)
		return t.fullTextSearch(ctx, query, limit)
	}
	defer rows.Close()

	return scanResults(rows)
}

// scanResults reads query rows into SearchResult slice.
func scanResults(rows interface {
	Close()
	Next() bool
	Scan(dest ...any) error
	Err() error
}) (json.RawMessage, error) {
	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.SessionID, &r.Source, &r.Digest, &r.CreatedAt, &r.Rank); err != nil {
			return nil, fmt.Errorf("scanning search result: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating search results: %w", err)
	}

	if results == nil {
		results = []SearchResult{}
	}

	return json.Marshal(results)
}
