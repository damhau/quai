import { DatabaseSync } from "node:sqlite";
import { randomUUID } from "node:crypto";
import type { ApprovalRule, FileChange, RenderedFile, Review } from "@quai/core";

// SQLite holds workflow state only (requests, reviews, audit trail, sync
// status). What is deployed, and how, is always read back from Git.

export type RequestKind = "create" | "update" | "delete" | "action";
export type RequestStatus = "pending" | "approved" | "committed" | "rejected" | "cancelled" | "expired" | "failed";

export interface RequestRecord {
  id: string;
  number: number;
  kind: RequestKind;
  action?: string;
  /** Blueprint or resource type name, and its version. */
  blueprint: string;
  blueprintVersion: string;
  /** Set for requests on a repo-defined resource type. */
  resourceType?: string;
  /** Where plan/apply runs (e.g. `envs/prod`), when the type declares it. */
  stack?: string;
  deployment: string;
  dir: string;
  requester: string;
  reason: string;
  inputs: Record<string, unknown>;
  actionInputs?: Record<string, unknown>;
  files: RenderedFile[];
  changes: FileChange[];
  /** Fingerprint of the deployment directory the change was computed against. */
  base: string;
  rules: ApprovalRule[];
  reviews: Review[];
  status: RequestStatus;
  commit?: string;
  error?: string;
  createdAt: string;
  updatedAt: string;
  expiresAt: string;
}

export interface EventRecord {
  id: number;
  requestId?: string;
  deployment?: string;
  type: string;
  actor: string;
  message: string;
  at: string;
}

export interface SyncRecord {
  deployment: string;
  revision?: string;
  sync: string;
  health: string;
  message?: string;
  at: string;
}

const JSON_COLS = ["inputs", "actionInputs", "files", "changes", "rules", "reviews"] as const;

export class Store {
  private db: DatabaseSync;

  constructor(path: string) {
    this.db = new DatabaseSync(path);
    this.db.exec(`
      PRAGMA journal_mode = WAL;
      CREATE TABLE IF NOT EXISTS requests (
        id TEXT PRIMARY KEY, number INTEGER UNIQUE, kind TEXT, action TEXT, blueprint TEXT, blueprintVersion TEXT,
        deployment TEXT, dir TEXT, requester TEXT, reason TEXT, inputs TEXT, actionInputs TEXT, files TEXT,
        changes TEXT, base TEXT, rules TEXT, reviews TEXT, status TEXT, "commit" TEXT, error TEXT,
        createdAt TEXT, updatedAt TEXT, expiresAt TEXT
      );
      CREATE INDEX IF NOT EXISTS requests_deployment ON requests(deployment);
      CREATE INDEX IF NOT EXISTS requests_status ON requests(status);
      CREATE TABLE IF NOT EXISTS events (
        id INTEGER PRIMARY KEY AUTOINCREMENT, requestId TEXT, deployment TEXT, type TEXT, actor TEXT, message TEXT, at TEXT
      );
      CREATE INDEX IF NOT EXISTS events_request ON events(requestId);
      CREATE INDEX IF NOT EXISTS events_deployment ON events(deployment);
      CREATE TABLE IF NOT EXISTS sync (
        deployment TEXT PRIMARY KEY, revision TEXT, sync TEXT, health TEXT, message TEXT, at TEXT
      );
    `);
    const cols = (this.db.prepare("PRAGMA table_info(requests)").all() as { name: string }[]).map((c) => c.name);
    for (const col of ["resourceType", "stack"]) if (!cols.includes(col)) this.db.exec(`ALTER TABLE requests ADD COLUMN "${col}" TEXT`);
  }

  private row(r: any): RequestRecord {
    const out: any = { ...r };
    for (const c of JSON_COLS) out[c] = r[c] ? JSON.parse(r[c]) : c === "actionInputs" ? undefined : [];
    for (const k of ["action", "commit", "error", "resourceType", "stack"]) if (out[k] === null) delete out[k];
    return out;
  }

  createRequest(r: Omit<RequestRecord, "id" | "number" | "reviews" | "createdAt" | "updatedAt" | "expiresAt">, ttlHours: number): RequestRecord {
    const now = new Date();
    const { n } = this.db.prepare("SELECT COALESCE(MAX(number), 0) + 1 AS n FROM requests").get() as { n: number };
    const rec: RequestRecord = {
      ...r,
      id: randomUUID(),
      number: n,
      reviews: [],
      createdAt: now.toISOString(),
      updatedAt: now.toISOString(),
      expiresAt: new Date(now.getTime() + ttlHours * 3600_000).toISOString(),
    };
    const cols = Object.keys(rec);
    this.db
      .prepare(`INSERT INTO requests (${cols.map((c) => `"${c}"`).join(",")}) VALUES (${cols.map(() => "?").join(",")})`)
      .run(...cols.map((c) => this.encode(c, (rec as any)[c])));
    return rec;
  }

  private encode(col: string, v: unknown): any {
    if ((JSON_COLS as readonly string[]).includes(col)) return v === undefined ? null : JSON.stringify(v);
    return v ?? null;
  }

  updateRequest(id: string, patch: Partial<RequestRecord>): RequestRecord {
    const full = { ...patch, updatedAt: new Date().toISOString() };
    const cols = Object.keys(full);
    this.db
      .prepare(`UPDATE requests SET ${cols.map((c) => `"${c}" = ?`).join(", ")} WHERE id = ?`)
      .run(...cols.map((c) => this.encode(c, (full as any)[c])), id);
    return this.getRequest(id)!;
  }

  getRequest(idOrNumber: string): RequestRecord | undefined {
    const r = /^\d+$/.test(idOrNumber)
      ? this.db.prepare("SELECT * FROM requests WHERE number = ?").get(Number(idOrNumber))
      : this.db.prepare("SELECT * FROM requests WHERE id = ?").get(idOrNumber);
    return r ? this.row(r) : undefined;
  }

  listRequests(filter: { status?: RequestStatus[]; deployment?: string; requester?: string; limit?: number } = {}): RequestRecord[] {
    const where: string[] = [];
    const args: any[] = [];
    if (filter.status?.length) {
      where.push(`status IN (${filter.status.map(() => "?").join(",")})`);
      args.push(...filter.status);
    }
    if (filter.deployment) {
      where.push("deployment = ?");
      args.push(filter.deployment);
    }
    if (filter.requester) {
      where.push("requester = ?");
      args.push(filter.requester);
    }
    const sql = `SELECT * FROM requests ${where.length ? `WHERE ${where.join(" AND ")}` : ""} ORDER BY number DESC LIMIT ${filter.limit ?? 200}`;
    return this.db.prepare(sql).all(...args).map((r) => this.row(r));
  }

  addEvent(e: Omit<EventRecord, "id" | "at">) {
    this.db
      .prepare("INSERT INTO events (requestId, deployment, type, actor, message, at) VALUES (?, ?, ?, ?, ?, ?)")
      .run(e.requestId ?? null, e.deployment ?? null, e.type, e.actor, e.message, new Date().toISOString());
  }

  listEvents(filter: { requestId?: string; deployment?: string; limit?: number } = {}): EventRecord[] {
    if (filter.requestId) return this.db.prepare("SELECT * FROM events WHERE requestId = ? ORDER BY id").all(filter.requestId) as any;
    if (filter.deployment)
      return this.db.prepare("SELECT * FROM events WHERE deployment = ? ORDER BY id DESC LIMIT ?").all(filter.deployment, filter.limit ?? 100) as any;
    return this.db.prepare("SELECT * FROM events ORDER BY id DESC LIMIT ?").all(filter.limit ?? 100) as any;
  }

  setSync(s: Omit<SyncRecord, "at">) {
    this.db
      .prepare(
        `INSERT INTO sync (deployment, revision, sync, health, message, at) VALUES (?, ?, ?, ?, ?, ?)
         ON CONFLICT(deployment) DO UPDATE SET revision = excluded.revision, sync = excluded.sync,
         health = excluded.health, message = excluded.message, at = excluded.at`,
      )
      .run(s.deployment, s.revision ?? null, s.sync, s.health, s.message ?? null, new Date().toISOString());
  }

  getSync(deployment: string): SyncRecord | undefined {
    return this.db.prepare("SELECT * FROM sync WHERE deployment = ?").get(deployment) as any;
  }

  close() {
    this.db.close();
  }
}
