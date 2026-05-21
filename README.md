# beluga-ext-history

Searchable history extension for Beluga. Builds session digests (messages only, tools stripped) and provides full-text or vector search over past conversations.

## How It Works

1. After sessions complete, a background process builds text digests — stripping all tool call/result noise and keeping only user and agent messages
2. Digests are stored in PostgreSQL with a `tsvector` column for full-text search
3. The `history_search` tool lets the agent find relevant past conversations
4. Optionally, configure an embedding model for semantic vector search via pgvector

## Tools

| Tool | Description |
|------|-------------|
| `history_search` | Search past session digests using full-text or vector search |

## Install

```bash
beluga extend install github.com/aspectrr/beluga-ext-history
```

## Config

### Full-text search only (default)

No extra config needed — works with plain PostgreSQL.

```yaml
extensions:
  searchable_history:
    enabled: true
```

### With embedding (semantic search)

Requires PostgreSQL with the [pgvector](https://github.com/pgvector/pgvector) extension.

```yaml
extensions:
  searchable_history:
    enabled: true
    embedding_model: "text-embedding-3-small"
    embedding_dimensions: 1536
```

When `embedding_model` is set, the extension:
- Adds an `embedding` column (vector type) to `session_digests`
- Creates an HNSW index for fast cosine similarity search
- Builds embeddings for new digests automatically
- Falls back to full-text search if embedding fails

### Migrating from FTS to vector search

If you already have digests built with full-text search and want to enable embeddings:

1. Install pgvector in your PostgreSQL instance
2. Add `embedding_model` and `embedding_dimensions` to your config
3. Restart Beluga — the migration will add the vector column automatically
4. Backfill existing digests by deleting and letting them rebuild:
   ```sql
   DELETE FROM session_digests;
   ```
   The digest loop will rebuild them with embeddings on the next tick (every 30s by default).

## Config Options

| Key | Required | Description |
|-----|----------|-------------|
| `embedding_model` | No | Embedding model name (enables pgvector mode) |
| `embedding_dimensions` | No | Vector dimensions (must match model output) |
| `digest_interval` | No | How often to check for new digests (default: 30s) |

## Files

- `extension.go` — Extension lifecycle (Init/Start/Stop), config parsing, digest loop
- `migration.go` — Database migrations (FTS base + optional vector upgrade)
- `search.go` — Full-text and vector search implementation
- `digest.go` — Digest builder (strips tool noise, keeps messages)
- `extension.yaml` — Manifest for `beluga extend install`
