import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import type { Blueprint } from "@quai/core";
import { useApi } from "../api";
import { Icon } from "../components/Icon";
import { Empty, ErrorNote } from "../components/Bits";
import { useMe } from "../App";

export function Catalog() {
  const me = useMe();
  const { data, error } = useApi<{ items: Blueprint[]; errors: { path: string; error: string }[] }>("/blueprints");
  const [q, setQ] = useState("");
  const [cat, setCat] = useState("All");
  const items = data?.items ?? [];
  const cats = useMemo(() => ["All", ...new Set(items.map((b) => b.metadata.category ?? "Other"))], [items]);
  const shown = items.filter((b) => {
    const hay = `${b.metadata.title} ${b.metadata.description ?? ""} ${(b.metadata.tags ?? []).join(" ")}`.toLowerCase();
    return (cat === "All" || (b.metadata.category ?? "Other") === cat) && hay.includes(q.toLowerCase());
  });

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <h1>Self-service catalog</h1>
          <p className="muted">Pick a blueprint, fill in the form, and Quai turns your request into a reviewed Git commit.</p>
        </div>
        {me.admin && (
          <Link className="btn" to="/designer">
            <Icon name="plus" /> New blueprint
          </Link>
        )}
      </header>
      <ErrorNote error={error} />
      {data?.errors.map((e) => (
        <div className="note warn" key={e.path}>
          <Icon name="alert" size={14} /> <code>{e.path}</code> is invalid and hidden from the catalog: {e.error}
        </div>
      ))}
      <div className="toolbar">
        <div className="search">
          <Icon name="search" />
          <input placeholder="Search blueprints" value={q} onChange={(e) => setQ(e.target.value)} aria-label="Search blueprints" />
        </div>
        <div className="chips">
          {cats.map((c) => (
            <button key={c} className={`chip ${c === cat ? "on" : ""}`} onClick={() => setCat(c)}>
              {c}
            </button>
          ))}
        </div>
      </div>
      {data && !shown.length && <Empty icon="search" title="No blueprints match" />}
      <div className="cards">
        {shown.map((b) => (
          <div className="card blueprint" key={b.metadata.name}>
            <div className="card-top">
              <div className="bp-icon">
                <Icon name={b.metadata.icon} size={20} />
              </div>
              <div className="card-meta">
                <span className="tag">{b.metadata.category ?? "Other"}</span>
                <span className="small muted">v{b.metadata.version ?? "0.0.0"}</span>
              </div>
            </div>
            <h3>{b.metadata.title}</h3>
            <p className="muted small grow">{b.metadata.description}</p>
            <div className="card-foot">
              <span className="small muted">
                {b.spec.resources.length} resource{b.spec.resources.length !== 1 ? "s" : ""} · {b.spec.actions?.length ?? 0} day-2 action
                {(b.spec.actions?.length ?? 0) !== 1 ? "s" : ""}
              </span>
              <div className="row">
                {me.admin && (
                  <Link className="btn ghost sm" to={`/designer/${b.metadata.name}`} aria-label={`Edit ${b.metadata.title}`}>
                    <Icon name="pen" size={14} />
                  </Link>
                )}
                <Link className="btn sm" to={`/request/${b.metadata.name}`}>
                  Request
                </Link>
              </div>
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}
