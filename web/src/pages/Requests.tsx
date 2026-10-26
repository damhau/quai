import { useEffect } from "react";
import { Link } from "react-router-dom";
import { ago, requestTitle, useApi, type RequestView } from "../api";
import { Empty, ErrorNote, Sha, StatusBadge } from "../components/Bits";
import { Icon } from "../components/Icon";

export function Requests({ view, onChange }: { view: "mine" | "inbox"; onChange?: () => void }) {
  const { data, error } = useApi<RequestView[]>(`/requests?view=${view}`, 4000);
  const inbox = view === "inbox";
  // Keep the sidebar badge in step with what this page shows.
  useEffect(() => {
    if (inbox && data) onChange?.();
  }, [inbox, data, onChange]);
  return (
    <div className="page">
      <header className="page-head">
        <div>
          <h1>{inbox ? "Approvals" : "My requests"}</h1>
          <p className="muted">
            {inbox
              ? "Requests waiting on you. You review the exact diff that will be committed — nothing else."
              : "Everything you asked for, from submission to commit."}
          </p>
        </div>
      </header>
      <ErrorNote error={error} />
      {data && !data.length && (
        <Empty icon={inbox ? "inbox" : "list"} title={inbox ? "Inbox zero" : "No requests yet"}>
          {inbox ? "Nothing needs your approval right now." : <Link to="/">Browse the catalog</Link>}
        </Empty>
      )}
      {!!data?.length && (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>#</th>
                <th>Request</th>
                <th>Blueprint</th>
                {inbox && <th>Requested by</th>}
                <th>Status</th>
                <th>Commit</th>
                <th>Updated</th>
              </tr>
            </thead>
            <tbody>
              {data.map((r) => (
                <tr key={r.id}>
                  <td className="muted">#{r.number}</td>
                  <td>
                    <Link to={`/requests/${r.id}`} className="strong">
                      {requestTitle(r)}
                    </Link>
                    {r.reason && <div className="small muted clip">{r.reason}</div>}
                  </td>
                  <td className="small">{r.blueprint}</td>
                  {inbox && (
                    <td>
                      <span className="principal">
                        <Icon name="user" size={12} />
                        {r.requester}
                      </span>
                    </td>
                  )}
                  <td>
                    <StatusBadge status={r.status} />
                  </td>
                  <td>
                    <Sha sha={r.commit} />
                  </td>
                  <td className="small muted">{ago(r.updatedAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
