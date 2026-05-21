package searchable_history

import (
	"context"
	"encoding/json"
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

// Config holds the extension configuration from beluga.yaml.
type Config struct {
	Enabled             bool   `json:"enabled"`
	EmbeddingModel      string `json:"embedding_model"`
	EmbeddingDimensions int    `json:"embedding_dimensions"`
	DigestInterval      string `json:"digest_interval"`
}

func (c Config) EmbeddingEnabled() bool {
	return c.EmbeddingModel != "" && c.EmbeddingDimensions > 0
}

func (c Config) DigestTickerDuration() time.Duration {
	if c.DigestInterval == "" {
		return 30 * time.Second
	}
	d, err := time.ParseDuration(c.DigestInterval)
	if err != nil {
		return 30 * time.Second
	}
	return d
}

// Extension builds session digests and searches them.
// Uses full-text search by default. When embedding_model is configured,
// switches to pgvector-based semantic search.
type Extension struct {
	db          *pgxpool.Pool
	events      *eventstore.Store
	cfg         Config
	embedClient Embedder
	logger      *slog.Logger
}

func (e *Extension) Name() string { return "searchable_history" }

func (e *Extension) Init(ctx extension.ExtensionContext) error {
	e.db = ctx.DB
	e.events = ctx.Events
	e.logger = ctx.Logger

	// Parse config.
	var cfg Config
	if err := json.Unmarshal(ctx.Config, &cfg); err != nil {
		return fmt.Errorf("searchable_history: parsing config: %w", err)
	}
	e.cfg = cfg

	// Run migration: create session_digests table (FTS always, vector if enabled).
	if err := e.migrate(context.Background()); err != nil {
		return fmt.Errorf("searchable_history migration: %w", err)
	}

	// If embedding is configured, initialize the embedder.
	if cfg.EmbeddingEnabled() {
		// TODO: Initialize embedding client from the configured model.
		// This will use Beluga's LLM config or a dedicated embedding endpoint.
		// For now, log a warning that the config is set but not yet wired.
		e.logger.Warn("embedding_model configured but embedding client not yet implemented",
			"model", cfg.EmbeddingModel,
			"dimensions", cfg.EmbeddingDimensions,
		)
	}

	// Register the history search tool.
	if err := ctx.Registry.Register(&HistorySearchTool{
		db:       e.db,
		embedder: e.embedClient,
		cfg:      e.cfg,
		logger:   e.logger,
	}); err != nil {
		return fmt.Errorf("registering history_search tool: %w", err)
	}

	mode := "full-text"
	if cfg.EmbeddingEnabled() {
		mode = "vector (embedding)"
	}

	e.logger.Info("searchable_history extension initialized",
		"mode", mode,
		"digest_interval", cfg.DigestTickerDuration(),
	)
	return nil
}

func (e *Extension) Start(ctx context.Context) error {
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
	ticker := time.NewTicker(e.cfg.DigestTickerDuration())
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
		events, err := e.events.GetEvents(ctx, p.id, 0, 10000)
		if err != nil {
			e.logger.Error("failed to get events for digest", "session_id", p.id, "error", err)
			continue
		}

		if len(events) == 0 {
			continue
		}

		digest := BuildDigest(events)
		if digest == "" {
			continue
		}

		// Build the insert — with or without embedding.
		var embedding []float64
		if e.embedClient != nil {
			embedding, err = e.embedClient.Embed(ctx, digest)
			if err != nil {
				e.logger.Error("failed to embed digest", "session_id", p.id, "error", err)
			}
		}

		if embedding != nil {
			_, err = e.db.Exec(ctx, `
				INSERT INTO session_digests (session_id, source, digest, embedding)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (session_id) DO NOTHING
			`, p.id, p.source, digest, formatVector(embedding))
		} else {
			_, err = e.db.Exec(ctx, `
				INSERT INTO session_digests (session_id, source, digest)
				VALUES ($1, $2, $3)
				ON CONFLICT (session_id) DO NOTHING
			`, p.id, p.source, digest)
		}
		if err != nil {
			e.logger.Error("failed to insert digest", "session_id", p.id, "error", err)
			continue
		}

		e.logger.Info("built digest for session",
			"session_id", p.id,
			"source", p.source,
			"digest_len", len(digest),
			"embedded", embedding != nil,
		)
	}
}

// formatVector formats a float64 slice as a pgvector literal string.
func formatVector(v []float64) string {
	s := "["
	for i, f := range v {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf("%f", f)
	}
	s += "]"
	return s
}
