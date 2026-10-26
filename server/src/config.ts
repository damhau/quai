import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { parse } from "yaml";
import type { Principal } from "@quai/core";

export interface QuaiConfig {
  port: number;
  dataDir: string;
  git: {
    remote: string;
    branch: string;
    committer: { name: string; email: string };
    /** Directory copied into the repository on first start, only if the repository is empty. */
    seed?: string;
    /** Pull commits made outside Quai this often (0 disables). */
    pullIntervalSeconds: number;
  };
  auth: {
    /**
     * `dev`: identity comes from the `x-quai-user` header (user switcher in the UI).
     * `proxy`: trust `x-forwarded-user` / `x-forwarded-groups` / `x-forwarded-email`
     * set by an authenticating proxy such as oauth2-proxy in front of Quai.
     */
    mode: "dev" | "proxy";
    users: Principal[];
  };
  /** Groups allowed to edit blueprints and act on any deployment. */
  adminGroups: string[];
  /** Pending requests expire after this many hours. */
  requestTtlHours: number;
  /** Shared secret for the sync-status webhook (Argo CD notifications, Flux alerts). */
  webhookToken?: string;
  /** Pretend a GitOps controller reconciled each commit (for demos without a cluster). */
  simulateSync: boolean;
  webDist?: string;
}

const root = resolve(import.meta.dirname, "../..");

const defaults = (): QuaiConfig => ({
  port: 8080,
  dataDir: resolve(root, "data"),
  git: {
    // Empty means a local bare repository inside dataDir (handy for trying Quai out).
    remote: "",
    branch: "main",
    committer: { name: "Quai", email: "quai@localhost" },
    seed: resolve(root, "examples/gitops-repo"),
    pullIntervalSeconds: 60,
  },
  auth: {
    mode: "dev",
    users: [
      { name: "alice", displayName: "Alice (platform admin)", email: "alice@example.com", groups: ["platform-admins", "developers"] },
      { name: "bob", displayName: "Bob (developer)", email: "bob@example.com", groups: ["developers"] },
      { name: "fiona", displayName: "Fiona (FinOps)", email: "fiona@example.com", groups: ["finops"] },
      { name: "dana", displayName: "Dana (DBA)", email: "dana@example.com", groups: ["dba", "developers"] },
      { name: "nora", displayName: "Nora (network)", email: "nora@example.com", groups: ["network"] },
    ],
  },
  adminGroups: ["platform-admins"],
  requestTtlHours: 24 * 7,
  simulateSync: true,
  webDist: resolve(root, "web/dist"),
});

function merge<T>(base: T, over: any): T {
  if (!over || typeof over !== "object" || Array.isArray(over)) return (over ?? base) as T;
  const out: any = { ...base };
  for (const [k, v] of Object.entries(over)) out[k] = merge((base as any)?.[k], v);
  return out;
}

export function loadConfig(overrides: Partial<QuaiConfig> | Record<string, unknown> = {}): QuaiConfig {
  let cfg = defaults();
  const file = process.env.QUAI_CONFIG ?? resolve(root, "quai.config.yaml");
  if (existsSync(file)) cfg = merge(cfg, parse(readFileSync(file, "utf8")));
  const env = process.env;
  cfg = merge(cfg, {
    port: env.PORT ? Number(env.PORT) : undefined,
    dataDir: env.QUAI_DATA_DIR,
    git: { remote: env.QUAI_GIT_REMOTE, branch: env.QUAI_GIT_BRANCH, seed: env.QUAI_GIT_SEED },
    auth: { mode: env.QUAI_AUTH_MODE },
    webhookToken: env.QUAI_WEBHOOK_TOKEN,
    simulateSync: env.QUAI_SIMULATE_SYNC === undefined ? undefined : env.QUAI_SIMULATE_SYNC === "true",
  });
  cfg = merge(cfg, overrides);
  cfg.dataDir = resolve(cfg.dataDir);
  if (!cfg.git.remote) cfg.git.remote = resolve(cfg.dataDir, "gitops.git");
  return cfg;
}
