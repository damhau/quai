import { useEffect, useState } from "react";
import { lineDiff, type DiffLine, type FileChange } from "@quai/core";
import { Link } from "react-router-dom";
import { Icon } from "./Icon";
import type { ApprovalStatus, RequestStatus, SyncView } from "../api";
import type { ApprovalRule } from "@quai/core";

const STATUS: Record<RequestStatus, { label: string; tone: string }> = {
  pending: { label: "Awaiting approval", tone: "warn" },
  approved: { label: "Approved", tone: "info" },
  committed: { label: "Committed", tone: "ok" },
  rejected: { label: "Rejected", tone: "bad" },
  cancelled: { label: "Cancelled", tone: "muted" },
  expired: { label: "Expired", tone: "muted" },
  failed: { label: "Failed", tone: "bad" },
};

export function StatusBadge({ status }: { status: RequestStatus }) {
  const s = STATUS[status];
  return <span className={`badge ${s.tone}`}>{s.label}</span>;
}

export function SyncBadge({ sync }: { sync?: SyncView }) {
  if (!sync) return <span className="badge muted">Unknown</span>;
  const tone = sync.health === "Healthy" ? "ok" : sync.health === "Degraded" || sync.health === "Missing" ? "bad" : sync.health === "Progressing" || sync.health === "Deleting" ? "info" : "muted";
  return (
    <span className={`badge ${tone}`} title={sync.message}>
      <span className="dot" />
      {sync.sync === "Synced" ? sync.health : `${sync.sync} · ${sync.health}`}
    </span>
  );
}

export function Sha({ sha }: { sha?: string }) {
  if (!sha) return null;
  return (
    <Link to={`/git?sha=${sha}`} className="sha">
      <Icon name="commit" size={12} />
      {sha.slice(0, 7)}
    </Link>
  );
}

export function Principal({ p }: { p: string }) {
  const [kind, name] = p.includes(":") ? p.split(":", 2) : ["group", p];
  return (
    <span className="principal">
      <Icon name={kind === "user" ? "user" : "shield"} size={12} />
      {name}
    </span>
  );
}

export function RulesPreview({ rules }: { rules: ApprovalRule[] }) {
  if (!rules.length)
    return (
      <div className="rules none">
        <Icon name="zap" size={14} /> No approval needed — this will be committed as soon as you submit.
      </div>
    );
  return (
    <div className="rules">
      {rules.map((r) => (
        <div className="rule" key={r.name}>
          <Icon name="shield" size={14} />
          <span>
            <b>{r.name}</b> — {r.min ?? 1} approval{(r.min ?? 1) > 1 ? "s" : ""} from {r.approvers.map((a) => <Principal key={a} p={a} />)}
          </span>
        </div>
      ))}
    </div>
  );
}

export function ApprovalChain({ approval }: { approval: ApprovalStatus[] }) {
  if (!approval.length) return <div className="muted small">Auto-approved: no rule matched.</div>;
  return (
    <div className="chain">
      {approval.map((r) => (
        <div className={`step ${r.satisfied ? "done" : ""}`} key={r.name}>
          <div className="step-icon">{r.satisfied ? <Icon name="check" size={14} /> : <Icon name="clock" size={14} />}</div>
          <div>
            <div className="step-title">{r.name}</div>
            <div className="small muted">
              {r.approvals.length}/{r.min} from {r.approvers.map((a) => <Principal key={a} p={a} />)}
              {r.approvals.length > 0 && <> · approved by {r.approvals.join(", ")}</>}
            </div>
          </div>
        </div>
      ))}
    </div>
  );
}

/** Keeps `context` unchanged lines around each change; `null` marks a fold. */
function foldContext(lines: DiffLine[], context: number): (DiffLine | null)[] {
  if (!Number.isFinite(context)) return lines;
  const keep = lines.map(() => false);
  lines.forEach((l, i) => {
    if (l.op !== " ") for (let j = Math.max(0, i - context); j <= Math.min(lines.length - 1, i + context); j++) keep[j] = true;
  });
  const out: (DiffLine | null)[] = [];
  lines.forEach((l, i) => {
    if (keep[i]) out.push(l);
    else if (out[out.length - 1] !== null) out.push(null);
  });
  return out;
}

export function ChangesView({ changes, collapsed = false }: { changes: FileChange[]; collapsed?: boolean }) {
  const [open, setOpen] = useState<Record<string, boolean>>({});
  useEffect(() => setOpen({}), [changes]);
  if (!changes.length) return <div className="muted small">No file changes.</div>;
  const added = changes.reduce((n, c) => n + lineDiff(c.before ?? "", c.after ?? "").filter((l) => l.op === "+").length, 0);
  const removed = changes.reduce((n, c) => n + lineDiff(c.before ?? "", c.after ?? "").filter((l) => l.op === "-").length, 0);
  return (
    <div className="changes">
      <div className="changes-summary small">
        {changes.length} file{changes.length > 1 ? "s" : ""} · <span className="plus">+{added}</span> <span className="minus">−{removed}</span>
      </div>
      {changes.map((c) => {
        const isOpen = open[c.path] ?? !collapsed;
        const lines = c.before !== undefined || c.after !== undefined ? lineDiff(c.before ?? "", c.after ?? "") : [];
        return (
          <div className="file" key={c.path}>
            <button className="file-head" onClick={() => setOpen({ ...open, [c.path]: !isOpen })} aria-expanded={isOpen}>
              <Icon name="chevron" size={12} className={isOpen ? "rot" : ""} />
              <span className={`op ${c.op}`}>{c.op === "add" ? "A" : c.op === "delete" ? "D" : "M"}</span>
              <code>{c.path}</code>
            </button>
            {isOpen && lines.length > 0 && (
              <pre className="diff">
                {foldContext(lines, c.op === "modify" ? 3 : Infinity).map((l, i) =>
                  l === null ? (
                    <div key={i} className="fold">
                      ⋯
                    </div>
                  ) : (
                    <div key={i} className={l.op === "+" ? "add" : l.op === "-" ? "del" : ""}>
                      <span className="gutter">{l.op}</span>
                      {l.text}
                    </div>
                  ),
                )}
              </pre>
            )}
          </div>
        );
      })}
    </div>
  );
}

export function Modal({ title, onClose, children, wide }: { title: string; onClose: () => void; children: React.ReactNode; wide?: boolean }) {
  useEffect(() => {
    const k = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", k);
    return () => window.removeEventListener("keydown", k);
  }, [onClose]);
  return (
    <div className="modal-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className={`modal ${wide ? "wide" : ""}`} role="dialog" aria-modal="true" aria-label={title}>
        <div className="modal-head">
          <h3>{title}</h3>
          <button className="icon-btn" onClick={onClose} aria-label="Close">
            <Icon name="x" />
          </button>
        </div>
        {children}
      </div>
    </div>
  );
}

export function Empty({ icon, title, children }: { icon: string; title: string; children?: React.ReactNode }) {
  return (
    <div className="empty">
      <Icon name={icon} size={28} />
      <h3>{title}</h3>
      {children && <div className="muted">{children}</div>}
    </div>
  );
}

export function ErrorNote({ error }: { error?: Error }) {
  if (!error) return null;
  const details = (error as any).details;
  return (
    <div className="note bad">
      <Icon name="alert" size={14} /> {error.message}
      {Array.isArray(details) && (
        <ul>
          {details.map((d: string) => (
            <li key={d}>{d}</li>
          ))}
        </ul>
      )}
    </div>
  );
}
