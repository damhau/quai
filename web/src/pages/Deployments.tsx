import { useState } from "react";
import { Link } from "react-router-dom";
import { ago, useApi, type DeploymentView } from "../api";
import { Empty, ErrorNote, Sha, SyncBadge } from "../components/Bits";
import { Icon } from "../components/Icon";
import { useMe } from "../App";

export function Deployments() {
  const me = useMe();
  const { data, error } = useApi<DeploymentView[]>("/deployments", 4000);
  const [mine, setMine] = useState(false);
  const rows = (data ?? []).filter((d) => !mine || d.record.metadata.owner === me.name);
  return (
    <div className="page">
      <header className="page-head">
        <div>
          <h1>Deployments</h1>
          <p className="muted">The inventory is read straight from Git: every directory with a <code>quai.yaml</code> is a deployment.</p>
        </div>
        <div className="chips">
          <button className={`chip ${!mine ? "on" : ""}`} onClick={() => setMine(false)}>
            All
          </button>
          <button className={`chip ${mine ? "on" : ""}`} onClick={() => setMine(true)}>
            Owned by me
          </button>
        </div>
      </header>
      <ErrorNote error={error} />
      {data && !rows.length && (
        <Empty icon="server" title="Nothing deployed yet">
          <Link to="/">Request something from the catalog</Link>
        </Empty>
      )}
      {!!rows.length && (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Blueprint</th>
                <th>Environment</th>
                <th>Owner</th>
                <th>Status</th>
                <th>Revision</th>
                <th>Updated</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((d) => (
                <tr key={d.name}>
                  <td>
                    <Link to={`/deployments/${d.name}`} className="strong">
                      {d.name}
                    </Link>
                    <div className="small muted">
                      <code>{d.dir}</code>
                    </div>
                  </td>
                  <td className="small">
                    {d.record.spec.blueprint} <span className="muted">v{d.record.spec.blueprintVersion}</span>
                  </td>
                  <td>{d.record.spec.inputs.environment ? <span className={`env ${d.record.spec.inputs.environment}`}>{String(d.record.spec.inputs.environment)}</span> : "—"}</td>
                  <td className="small">{d.record.metadata.owner}</td>
                  <td>
                    <SyncBadge sync={d.sync} />
                    {d.openRequests > 0 && (
                      <span className="badge warn" style={{ marginLeft: 6 }}>
                        <Icon name="clock" size={11} /> {d.openRequests} open
                      </span>
                    )}
                  </td>
                  <td>
                    <Sha sha={d.sync?.revision} />
                  </td>
                  <td className="small muted">{ago(d.sync?.at) || d.record.metadata.updatedAt}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
