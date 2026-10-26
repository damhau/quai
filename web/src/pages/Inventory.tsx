import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useApi, type InventoryItem } from "../api";
import { Empty, ErrorNote, Sha, SyncBadge } from "../components/Bits";
import { Icon } from "../components/Icon";
import { useMe } from "../App";

export function Inventory() {
  const me = useMe();
  const { data, error } = useApi<InventoryItem[]>("/inventory", 5000);
  const [mine, setMine] = useState(false);
  const [source, setSource] = useState("All");
  const [q, setQ] = useState("");
  const sources = useMemo(() => ["All", ...new Set((data ?? []).map((d) => d.sourceTitle))], [data]);
  const rows = (data ?? []).filter(
    (d) => (!mine || d.owner === me.name) && (source === "All" || d.sourceTitle === source) && `${d.name} ${d.path} ${d.owner ?? ""} ${d.env ?? ""}`.toLowerCase().includes(q.toLowerCase()),
  );

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <h1>Inventory</h1>
          <p className="muted">Read from Git: blueprint deployments, plus every file that matches a resource type, including ones that existed before Quai.</p>
        </div>
      </header>
      <ErrorNote error={error} />
      <div className="toolbar">
        <div className="search">
          <Icon name="search" />
          <input placeholder="Search name, path, owner" value={q} onChange={(e) => setQ(e.target.value)} aria-label="Search inventory" />
        </div>
        <div className="chips">
          {sources.map((s) => (
            <button key={s} className={`chip ${s === source ? "on" : ""}`} onClick={() => setSource(s)}>
              {s}
            </button>
          ))}
          <button className={`chip ${mine ? "on" : ""}`} onClick={() => setMine(!mine)}>
            Owned by me
          </button>
        </div>
      </div>
      {data && !rows.length && (
        <Empty icon="server" title="Nothing here">
          <Link to="/">Request something from the catalog</Link>
        </Empty>
      )}
      {!!rows.length && (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Type</th>
                <th>Environment</th>
                <th>Owner</th>
                <th>Status</th>
                <th>Revision</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((d) => (
                <tr key={d.id}>
                  <td>
                    <Link to={d.href} className="strong">
                      {d.name}
                    </Link>
                    <div className="small muted">
                      <code>{d.path}</code>
                    </div>
                  </td>
                  <td className="small">
                    <span className="row">
                      <Icon name={d.icon ?? (d.kind === "deployment" ? "layers" : "file")} size={14} />
                      {d.sourceTitle}
                    </span>
                  </td>
                  <td>{d.env ? <span className={`env ${d.env}`}>{d.env}</span> : "—"}</td>
                  <td className="small">{d.owner ?? <span className="muted">—</span>}</td>
                  <td>
                    {d.error ? (
                      <span className="badge bad" title={d.error}>
                        Unparseable
                      </span>
                    ) : (
                      <SyncBadge sync={d.sync} fallback={d.kind === "resource" ? "In repo" : "Unknown"} />
                    )}
                    {d.openRequests > 0 && (
                      <span className="badge warn" style={{ marginLeft: 6 }}>
                        <Icon name="clock" size={11} /> {d.openRequests} open
                      </span>
                    )}
                  </td>
                  <td>
                    <Sha sha={d.sync?.revision} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
