package searchable_history

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/collinpfeifer/beluga/pkg/eventstore"
	"github.com/collinpfeifer/beluga/pkg/extension"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Embedder is the interface for optional embedding support.
// If nil, the extension uses full-text search only.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float64, error)
}

// Extension builds session digests and searches them using full-text search.
type Extension struct {
	db          *pgxpool.Pool
	events      *eventstore.Store
	embedClient Embedder
	logger      *slog.Logger
}

func (e *Extension) Name() string { return "searchable_history" }

func (e *Extension) Init(ctx extension.ExtensionContext) error {
	e.db = ctx.DB
	e.events = ctx.Events
	e.logger = ctx.Logger

	// Run migration: create session_digests table.
	if err := e.migrate(context.Background()); err != nil {
		return fmt.Errorf("searchable_history migration: %w", err)
	}

	// Embedding support is optional. For now, leave embedClient nil.
	// Future: check config for embedding model and initialize Embedder.

	// Register the history search tool.
	if err := ctx.Registry.Register(&HistorySearchTool{
		db:     e.db,
		logger: e.logger,
	}); err != nil {
		return fmt.Errorf("registering history_search tool: %w", err)
	}

	e.logger.Info("searchable_history extension initialized")
	return nil
}

func (e *Extension) Start(ctx context.Context) error {
	// Start background goroutine to build digests for completed sessions.
	go e.digestLoop(ctx)

	<-ctx.Done()
	return nil
}

func (e *Extension) Stop(ctx context.Context) error {
	return nil
}

// digestLoop periodically checks for completed sessions without digests
// and builds them.
func (e *Extension) digestLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Run once immediately on start.
	e.buildPendingDigests(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.buildPendingDigests(ctx)
		}
	}
}

// buildPendingDigests finds completed sessions without digests and builds them.
func (e *Extension) buildPendingDigests(ctx context.Context) {
	// Find sessions that are completed but don't have a digest yet.
	rows, err := e.db.Query(ctx, `
		SELECT s.id, s.source
		FROM sessions s
		LEFT JOIN session_digests sd ON s.id = sd.session_id
		WHERE s.status = 'completed' AND sd.session_id IS NULL
		ORDER BY s.updated_at DESC
		LIMIT 50
	`)
	if err != nil {
		e.logger.Error("failed to query sessions needing digests", "error", err)
		return
	}
	defer rows.Close()

	type pendingSession struct {
		id     string
		source string
	}
	var pending []pendingSession

	for rows.Next() {
		var p pendingSession
		if err := rows.Scan(&p.id, &p.source); err != nil {
			e.logger.Error("failed to scan pending session", "error", err)
			return
		}
		pending = append(pending, p)
	}
	if err := rows.Err(); err != nil {
		e.logger.Error("failed to iterate pending sessions", "error", err)
		return
	}

	if len(pending) == 0 {
		return
	}

	e.logger.Info("building digests for completed sessions", "count", len(pending))

	for _, p := range pending {
		// Load all events for the session.
		events, err := e.events.GetEvents(ctx, p.id, 0, 10000)
		if err != nil {
			e.logger.Error("failed to get events for digest", "session_id", p.id, "error", err)
			continue
		}

		if len(events) == 0 {
			continue
		}

		// Build digest (messages only, tools stripped).
		digest := BuildDigest(events)
		if digest == "" {
			continue
		}

		// Insert the digest.
		_, err = e.db.Exec(ctx, `
			INSERT INTO session_digests (session_id, source, digest)
			VALUES ($1, $2, $3)
			ON CONFLICT (session_id) DO NOTHING
		`, p.id, p.source, digest)
		if err != nil {
			e.logger.Error("failed to insert digest", "session_id", p.id, "error", err)
			continue
		}

		e.logger.Info("built digest for session",
			"session_id", p.id,
			"source", p.source,
			"digest_len", len(digest),
		)
	}
}
