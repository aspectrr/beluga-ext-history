// ── Searchable History Extension ──────────────────────────────
// Ported from the original Go implementation.
//
// Builds session digests (user+agent messages only) for completed sessions
// and exposes a history_search tool for FTS/vector search.

import { sql } from "drizzle-orm";
import type {
	Extension,
	ExtensionContext,
	Tool,
	ToolDef,
	ToolContext,
	EventType as EventTypeType,
} from "@aspectrr/beluga-sdk";

// EventType is a const object in the SDK; destructure values we need
import { EventType } from "@aspectrr/beluga-sdk";

// ── Config ─────────────────────────────────────────────────────

interface HistoryConfig {
	enabled: boolean;
	embedding_model?: string;
	embedding_dimensions?: number;
	digest_interval?: string;
}

function parseConfig(raw: Record<string, unknown>): HistoryConfig {
	return {
		enabled: raw.enabled !== false,
		embedding_model: raw.embedding_model as string | undefined,
		embedding_dimensions: raw.embedding_dimensions as number | undefined,
		digest_interval: (raw.digest_interval as string) || "30s",
	};
}

function parseInterval(s: string): number {
	const match = s.match(/^(\d+)(s|m|h)$/);
	if (!match) return 30_000;
	const n = parseInt(match[1]);
	if (match[2] === "s") return n * 1000;
	if (match[2] === "m") return n * 60_000;
	if (match[2] === "h") return n * 3_600_000;
	return 30_000;
}

// ── Types ──────────────────────────────────────────────────────

interface SearchResult {
	session_id: string;
	source: string;
	digest: string;
	created_at: string;
	rank: number;
}

interface Embedder {
	embed(text: string): Promise<number[]>;
}

// ── Digest builder ─────────────────────────────────────────────

function buildDigest(
	events: Array<{ type: string; data: Record<string, unknown> }>,
): string {
	const lines: string[] = [];
	for (const event of events) {
		if (event.type === EventType.UserMessage) {
			const content = event.data?.content as string;
			if (content) lines.push(`User: ${content}`);
		} else if (event.type === EventType.AgentMessage) {
			const content = event.data?.content as string;
			if (content) lines.push(`Agent: ${content}`);
		}
	}
	return lines.join("\n");
}

// ── HistorySearchTool ──────────────────────────────────────────

class HistorySearchTool implements Tool {
	private db: import("@aspectrr/beluga-sdk").ExtDB;
	private embedder: Embedder | null;

	constructor(db: import("@aspectrr/beluga-sdk").ExtDB, embedder: Embedder | null) {
		this.db = db;
		this.embedder = embedder;
	}

	definition(): ToolDef {
		return {
			name: "history_search",
			description:
				"Search past session history for relevant context. Returns matching session digests containing the conversation (user and agent messages only).",
			parameters: {
				type: "object",
				properties: {
					query: {
						type: "string",
						description: "Keywords or phrases to search for",
					},
					limit: { type: "integer", description: "Maximum results to return" },
				},
				required: ["query"],
			},
		};
	}

	async execute(
		args: Record<string, unknown>,
		_ctx: ToolContext,
	): Promise<Record<string, unknown>> {
		if (process.env.BELUGA_DRY_RUN === "true") {
			return {
				results: [
					{
						session_id: "00000000-0000-0000-0000-000000000000",
						source: "cli",
						digest: "User: example query\nAgent: example response",
						created_at: new Date().toISOString(),
						rank: 0.5,
					},
				],
			};
		}

		const query = args.query as string;
		const limit = (args.limit as number) || 5;

		// Try vector search if embedder available
		if (this.embedder) {
			try {
				const results = await this.vectorSearch(query, limit);
				if (results.length > 0) return { results };
			} catch {
				// Fall through to FTS
			}
		}

		const results = await this.fullTextSearch(query, limit);
		return { results };
	}

	private async fullTextSearch(
		query: string,
		limit: number,
	): Promise<SearchResult[]> {
		const rows = await this.db.executeSql(sql`
			SELECT session_id, source, digest, created_at,
				ts_rank(digest_tsv, plainto_tsquery('english', ${query})) AS rank
			FROM session_digests
			WHERE digest_tsv @@ plainto_tsquery('english', ${query})
			ORDER BY rank DESC
			LIMIT ${limit}
		`);
		return (rows as unknown as Record<string, unknown>[]).map((r) => ({
			session_id: r.session_id as string,
			source: r.source as string,
			digest: r.digest as string,
			created_at: String(r.created_at),
			rank: Number(r.rank),
		}));
	}

	private async vectorSearch(
		query: string,
		limit: number,
	): Promise<SearchResult[]> {
		const vec = await this.embedder!.embed(query);
		const vecLiteral = `[${vec.join(",")}]`;
		const rows = await this.db.executeSql(sql`
			SELECT session_id, source, digest, created_at,
				1 - (embedding <=> ${sql.raw(`'${vecLiteral}'`)}::vector) AS rank
			FROM session_digests
			WHERE embedding IS NOT NULL
			ORDER BY embedding <=> ${sql.raw(`'${vecLiteral}'`)}::vector
			LIMIT ${limit}
		`);
		return (rows as unknown as Record<string, unknown>[]).map((r) => ({
			session_id: r.session_id as string,
			source: r.source as string,
			digest: r.digest as string,
			created_at: String(r.created_at),
			rank: Number(r.rank),
		}));
	}
}

// ── Migration ──────────────────────────────────────────────────

async function migrate(db: import("@aspectrr/beluga-sdk").ExtDB, cfg: HistoryConfig): Promise<void> {
	await db.executeSql(sql`
		CREATE TABLE IF NOT EXISTS session_digests (
			session_id  UUID PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
			source      TEXT,
			digest      TEXT NOT NULL,
			digest_tsv  TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', digest)) STORED,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`);
	await db.executeSql(sql`
		CREATE INDEX IF NOT EXISTS session_digests_tsv_idx ON session_digests USING GIN (digest_tsv)
	`);

	// Optional pgvector column
	if (cfg.embedding_model && cfg.embedding_dimensions) {
		const dims = cfg.embedding_dimensions;
		try {
			await db.executeSql(sql`CREATE EXTENSION IF NOT EXISTS vector`);
			await db.executeSql(sql`
				ALTER TABLE session_digests ADD COLUMN IF NOT EXISTS embedding vector(${sql.raw(String(dims))})
			`);
			await db.executeSql(sql`
				CREATE INDEX IF NOT EXISTS session_digests_embedding_idx
				ON session_digests USING hnsw (embedding vector_cosine_ops)
			`);
		} catch {
			// pgvector not available — continue without vector support
		}
	}
}

// ── Extension ──────────────────────────────────────────────────

class HistoryExtension implements Extension {
	name = "searchable_history";
	private cfg!: HistoryConfig;
	private db!: import("@aspectrr/beluga-sdk").ExtDB;
	private events!: import("@aspectrr/beluga-sdk").EventStore;
	private intervalId?: ReturnType<typeof setInterval>;

	async init(ctx: ExtensionContext): Promise<void> {
		this.cfg = parseConfig(ctx.config);
		this.db = ctx.db;
		this.events = ctx.events;

		await migrate(this.db, this.cfg);

		// TODO: Wire embedder when LLM embedding support is available
		const embedder: Embedder | null = null;
		if (this.cfg.embedding_model && this.cfg.embedding_dimensions) {
			ctx.logger.warn(
				"embedding configured but no embed client wired yet — using FTS only",
			);
		}

		ctx.registry.register(new HistorySearchTool(this.db, embedder));
		const mode = embedder ? "vector" : "full-text";
		ctx.logger.info({ mode }, "history extension initialized");
	}

	async start(signal: AbortSignal): Promise<void> {
		const intervalMs = parseInterval(this.cfg.digest_interval || "30s");
		this.buildPendingDigests().catch(() => {});

		this.intervalId = setInterval(() => {
			this.buildPendingDigests().catch(() => {});
		}, intervalMs);

		signal.addEventListener("abort", () => {
			if (this.intervalId) clearInterval(this.intervalId);
		});
	}

	async stop(): Promise<void> {
		if (this.intervalId) clearInterval(this.intervalId);
	}

	private async buildPendingDigests(): Promise<void> {
		const result = await this.db.executeSql(sql`
			SELECT s.id, s.source
			FROM sessions s
			LEFT JOIN session_digests d ON s.id = d.session_id
			WHERE s.status = 'completed' AND d.session_id IS NULL
			ORDER BY s.updated_at DESC
			LIMIT 50
		`);

		for (const row of result as unknown as Array<{
			id: string;
			source: string;
		}>) {
			try {
				const evts = await this.events.getEvents(row.id, 0, 10000);
				const dig = buildDigest(evts);
				if (!dig) continue;

				await this.db.executeSql(sql`
					INSERT INTO session_digests (session_id, source, digest)
					VALUES (${row.id}, ${row.source}, ${dig})
					ON CONFLICT (session_id) DO NOTHING
				`);
			} catch {
				// Skip failed sessions
			}
		}
	}
}

export default new HistoryExtension();
