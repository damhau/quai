import { useCallback, useEffect, useState } from "react";
import type { ApprovalRule, Blueprint, DeploymentRecord, FileChange, InputDef, Principal, RenderedFile, ActionDef } from "@quai/core";

export class ApiError extends Error {
  constructor(public status: number, message: string, public fields: Record<string, string> = {}, public details?: unknown) {
    super(message);
  }
}

const USER_KEY = "quai.user";
export const currentUserName = () => {
  try {
    return localStorage.getItem(USER_KEY) ?? "";
  } catch {
    return "";
  }
};
export const setCurrentUserName = (name: string) => {
  try {
    localStorage.setItem(USER_KEY, name);
  } catch {
    // Private mode: the switcher just won't persist.
  }
};

export async function api<T = any>(path: string, opts: { method?: string; body?: unknown } = {}): Promise<T> {
  const headers: Record<string, string> = {};
  const user = currentUserName();
  if (user) headers["x-quai-user"] = user;
  if (opts.body !== undefined) headers["content-type"] = "application/json";
  const res = await fetch(`/api${path}`, { method: opts.method ?? (opts.body ? "POST" : "GET"), headers, body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined });
  const text = await res.text();
  const data = text && res.headers.get("content-type")?.includes("json") ? JSON.parse(text) : text;
  if (!res.ok) {
    const details = (data as any)?.details;
    throw new ApiError(res.status, (data as any)?.error ?? res.statusText, details?.fields ?? {}, details);
  }
  return data as T;
}

/** Fetch-on-mount with manual reload and optional polling. */
export function useApi<T>(path: string | null, pollMs?: number) {
  const [data, setData] = useState<T | undefined>();
  const [error, setError] = useState<Error | undefined>();
  const [loading, setLoading] = useState(true);
  const load = useCallback(async () => {
    if (!path) return;
    try {
      setData(await api<T>(path));
      setError(undefined);
    } catch (e) {
      setError(e as Error);
    } finally {
      setLoading(false);
    }
  }, [path]);
  useEffect(() => {
    setLoading(true);
    load();
    if (!pollMs) return;
    const t = setInterval(load, pollMs);
    return () => clearInterval(t);
  }, [load, pollMs]);
  return { data, error, loading, reload: load };
}

export interface Me extends Principal {
  admin: boolean;
  authMode: "dev" | "proxy";
}

export interface ApprovalStatus extends ApprovalRule {
  min: number;
  approvals: string[];
  satisfied: boolean;
}

export interface Review {
  reviewer: string;
  decision: "approve" | "deny";
  comment?: string;
  at: string;
}

export type RequestStatus = "pending" | "approved" | "committed" | "rejected" | "cancelled" | "expired" | "failed";

export interface RequestView {
  id: string;
  number: number;
  kind: "create" | "update" | "delete" | "action";
  action?: string;
  blueprint: string;
  blueprintVersion: string;
  deployment: string;
  dir: string;
  requester: string;
  reason: string;
  inputs: Record<string, unknown>;
  actionInputs?: Record<string, unknown>;
  changes: FileChange[];
  files?: RenderedFile[];
  reviews: Review[];
  status: RequestStatus;
  commit?: string;
  error?: string;
  createdAt: string;
  updatedAt: string;
  expiresAt: string;
  approval: ApprovalStatus[];
  canReview: boolean;
  canCancel: boolean;
  events?: EventView[];
}

export interface EventView {
  id: number;
  requestId?: string;
  deployment?: string;
  type: string;
  actor: string;
  message: string;
  at: string;
}

export interface SyncView {
  revision?: string;
  sync: string;
  health: string;
  message?: string;
  at: string;
}

export interface DeploymentView {
  name: string;
  dir: string;
  record: DeploymentRecord;
  sync?: SyncView;
  openRequests: number;
}

export interface DeploymentDetail extends DeploymentView {
  blueprint?: Blueprint;
  actions: ActionDef[];
  files: RenderedFile[];
  history: CommitView[];
  requests: RequestView[];
  events: EventView[];
  canOperate: boolean;
}

export interface CommitView {
  sha: string;
  author: string;
  email: string;
  date: string;
  subject: string;
  body: string;
}

export interface PlanView {
  kind: string;
  deployment: string;
  dir: string;
  inputs: Record<string, unknown>;
  changes: FileChange[];
  rules: ApprovalRule[];
}

export interface ComponentView {
  type: string;
  title: string;
  description: string;
  category: string;
  icon: string;
  color: string;
  properties: InputDef[];
  publishesSecret: boolean;
}

export const ago = (iso?: string) => {
  if (!iso) return "";
  const s = Math.round((Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 0) {
    const f = -s;
    if (f < 3600) return `in ${Math.round(f / 60)}m`;
    if (f < 86400) return `in ${Math.round(f / 3600)}h`;
    return `in ${Math.round(f / 86400)}d`;
  }
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  return `${Math.round(s / 86400)}d ago`;
};

export const requestTitle = (r: Pick<RequestView, "kind" | "action" | "deployment">) =>
  r.kind === "create" ? `Create ${r.deployment}` : r.kind === "delete" ? `Delete ${r.deployment}` : r.kind === "update" ? `Update ${r.deployment}` : `${r.action} ${r.deployment}`;
