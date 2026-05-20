# beluga-ext-history

Searchable history extension for Beluga. Builds session digests (messages only, tools stripped) and provides full-text search over past sessions.

## How It Works

1. After sessions complete, a background process builds text digests — stripping all tool call/result noise and keeping only user and agent messages
2. Digests are stored in PostgreSQL with a `tsvector` column for full-text search
3. The `history_search` tool lets the agent find relevant past conversations

## Tools

| Tool | Description |
|------|-------------|
| `history_search` | Search past session digests using full-text search |

## Install

```bash
beluga extend install github.com/collinpfeifer/beluga-ext-history
```

## Config

```yaml
extensions:
  searchable_history:
    enabled: true
```

## Migration

On first init, creates a `session_digests` table with a GIN index on the tsvector column. Requires the `sessions` table from Beluga core.

## Embedding (Future)

The extension supports an optional `Embedder` interface for vector-based search. When nil (default), falls back to PostgreSQL full-text search. Future versions can configure an embedding model for semantic search.
