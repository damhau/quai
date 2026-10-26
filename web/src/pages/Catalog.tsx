import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import type { Blueprint } from "@quai/core";
import { useApi, type TypeView } from "../api";
import { Icon } from "../components/Icon";
import { Empty, ErrorNote } from "../components/Bits";
import { useMe } from "../App";

interface Entry {
  key: string;
  kind: "blueprint" | "type";
  title: string;
  description?: string;
  icon?: string;
  category: string;
  version: string;
  tags: string[];
  href: string;
  editHref?: string;
  meta: string;
}

const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"}`;

export function Catalog() {
  const me = useMe();
  const blueprints = useApi<{ items: Blueprint[]; errors: { path: string; error: string }[] }>("/blueprints");
  const types = useApi<{ items: TypeView[]; errors: { path: string; error: string }[] }>("/types");
  const [q, setQ] = useState("");
  const [cat, setCat] = useState("All");

  const entries: Entry[] = useMemo(
    () => [
      ...(blueprints.data?.items ?? []).map((b) => ({
        key: `bp:${b.metadata.name}`,
        kind: "blueprint" as const,
        title: b.metadata.title,
        description: b.metadata.description,
        icon: b.metadata.icon,
        category: b.metadata.category ?? "Other",
        version: b.metadata.version ?? "0.0.0",
        tags: b.metadata.tags ?? [],
        href: `/request/${b.metadata.name}`,
        editHref: `/designer/${b.metadata.name}`,
        meta: `${plural(b.spec.resources.length, "resource")} · ${plural(b.spec.actions?.length ?? 0, "day-2 action")}`,
      })),
      ...(types.data?.items ?? []).map((t) => ({
        key: `type:${t.metadata.name}`,
        kind: "type" as const,
        title: t.metadata.title,
        description: t.metadata.description,
        icon: t.metadata.icon,
        category: t.metadata.category ?? "Other",
        version: t.metadata.version ?? "0.0.0",
        tags: t.metadata.tags ?? [],
        href: `/request/type/${t.metadata.name}`,
        meta: `${t.count ?? 0} in the repo · ${plural((t.spec.actions?.length ?? 0) + 2, "day-2 action")}`,
      })),
    ],
    [blueprints.data, types.data],
  );
  const cats = useMemo(() => ["All", ...new Set(entries.map((e) => e.category))], [entries]);
  const shown = entries.filter((e) => (cat === "All" || e.category === cat) && `${e.title} ${e.description ?? ""} ${e.tags.join(" ")}`.toLowerCase().includes(q.toLowerCase()));
  const problems = [...(blueprints.data?.errors ?? []), ...(types.data?.errors ?? [])];
  const loaded = blueprints.data && types.data;

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <h1>Self-service catalog</h1>
          <p className="muted">Pick an item, fill in the form, and Quai turns your request into a reviewed Git commit.</p>
        </div>
        {me.admin && (
          <Link className="btn" to="/designer">
            <Icon name="plus" /> New blueprint
          </Link>
        )}
      </header>
      <ErrorNote error={blueprints.error ?? types.error} />
      {problems.map((e) => (
        <div className="note warn" key={e.path}>
          <Icon name="alert" size={14} /> <code>{e.path}</code> is invalid and hidden from the catalog: {e.error}
        </div>
      ))}
      <div className="toolbar">
        <div className="search">
          <Icon name="search" />
          <input placeholder="Search the catalog" value={q} onChange={(e) => setQ(e.target.value)} aria-label="Search the catalog" />
        </div>
        <div className="chips">
          {cats.map((c) => (
            <button key={c} className={`chip ${c === cat ? "on" : ""}`} onClick={() => setCat(c)}>
              {c}
            </button>
          ))}
        </div>
      </div>
      {loaded && !shown.length && <Empty icon="search" title="Nothing matches" />}
      <div className="cards">
        {shown.map((e) => (
          <div className="card blueprint" key={e.key}>
            <div className="card-top">
              <div className="bp-icon">
                <Icon name={e.icon} size={20} />
              </div>
              <div className="card-meta">
                <span className="tag">{e.category}</span>
                <span className="small muted">
                  {e.kind === "type" ? "repo type" : "blueprint"} · v{e.version}
                </span>
              </div>
            </div>
            <h3>{e.title}</h3>
            <p className="muted small grow">{e.description}</p>
            <div className="card-foot">
              <span className="small muted">{e.meta}</span>
              <div className="row">
                {me.admin && e.editHref && (
                  <Link className="btn ghost sm" to={e.editHref} aria-label={`Edit ${e.title}`}>
                    <Icon name="pen" size={14} />
                  </Link>
                )}
                <Link className="btn sm" to={e.href}>
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
