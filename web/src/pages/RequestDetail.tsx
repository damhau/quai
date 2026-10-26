import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api, ApiError, ago, requestTitle, useApi, type RequestView } from "../api";
import { ApprovalChain, ChangesView, ErrorNote, Sha, StatusBadge } from "../components/Bits";
import { Icon } from "../components/Icon";
import { useMe } from "../App";

export function RequestDetail({ onChange }: { onChange?: () => void }) {
  const { id } = useParams();
  const me = useMe();
  const { data: r, error, reload } = useApi<RequestView>(`/requests/${id}`, 4000);
  const [comment, setComment] = useState("");
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<Error>();
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});

  const act = async (path: string, body: unknown = {}) => {
    setBusy(true);
    setActionError(undefined);
    setFieldErrors({});
    try {
      await api(`/requests/${id}/${path}`, { body });
      setComment("");
      await reload();
      onChange?.();
    } catch (e) {
      if (e instanceof ApiError) setFieldErrors(e.fields);
      setActionError(e as Error);
    } finally {
      setBusy(false);
    }
  };

  if (error) return <div className="page"><ErrorNote error={error} /></div>;
  if (!r) return <div className="page muted">Loading…</div>;
  const inputs = r.kind === "action" ? r.actionInputs ?? {} : r.inputs;

  return (
    <div className="page">
      <div className="crumb small muted">
        <Link to="/requests">Requests</Link> / #{r.number}
      </div>
      <header className="page-head">
        <div>
          <h1 className="row">
            {requestTitle(r)} <StatusBadge status={r.status} />
          </h1>
          <p className="muted">
            <b>{r.requester}</b> requested this {ago(r.createdAt)} from blueprint <b>{r.blueprint}</b> v{r.blueprintVersion}
            {r.kind !== "create" && (
              <>
                {" "}
                on <Link to={`/deployments/${r.deployment}`}>{r.deployment}</Link>
              </>
            )}
            {r.status === "pending" && <> · expires {ago(r.expiresAt)}</>}
          </p>
        </div>
        {r.commit && (
          <div className="committed">
            <Icon name="git" /> <Sha sha={r.commit} />
          </div>
        )}
      </header>

      {r.status === "failed" && (
        <div className="note bad">
          <Icon name="alert" size={14} /> Commit failed: {r.error}
          {(r.requester === me.name || me.admin) && (
            <button className="btn sm" style={{ marginLeft: 12 }} disabled={busy} onClick={() => act("retry")}>
              <Icon name="refresh" size={14} /> Retry commit
            </button>
          )}
        </div>
      )}

      <div className="split">
        <div className="panel">
          <h2>Reason</h2>
          <p>{r.reason || <span className="muted">No reason given.</span>}</p>
          <h2>{r.kind === "action" ? `Action inputs (${r.action})` : "Inputs"}</h2>
          <dl className="kv">
            {Object.entries(inputs).map(([k, v]) => (
              <div key={k}>
                <dt>{k}</dt>
                <dd>
                  <code>{JSON.stringify(v)}</code>
                </dd>
              </div>
            ))}
          </dl>
          <h2>
            Diff <span className="small muted">— exactly what will land in <code>{r.dir}/</code></span>
          </h2>
          <ChangesView changes={r.changes} />
        </div>
        <aside className="panel">
          <h2>Approval</h2>
          <ApprovalChain approval={r.approval} />
          {r.reviews.length > 0 && (
            <>
              <h3>Reviews</h3>
              {r.reviews.map((x) => (
                <div className={`review ${x.decision}`} key={x.reviewer + x.at}>
                  <Icon name={x.decision === "approve" ? "check" : "x"} size={14} />
                  <div>
                    <b>{x.reviewer}</b> {x.decision === "approve" ? "approved" : "denied"} <span className="muted small">{ago(x.at)}</span>
                    {x.comment && <div className="small">{x.comment}</div>}
                  </div>
                </div>
              ))}
            </>
          )}
          {r.canReview && (
            <div className="review-box">
              <h3>Your review</h3>
              <textarea rows={3} placeholder="Comment (required to deny)" value={comment} onChange={(e) => setComment(e.target.value)} aria-label="Review comment" />
              {fieldErrors.comment && <div className="error">{fieldErrors.comment}</div>}
              <div className="row">
                <button className="btn primary" disabled={busy} onClick={() => act("review", { decision: "approve", comment })}>
                  <Icon name="check" /> Approve
                </button>
                <button className="btn danger" disabled={busy} onClick={() => act("review", { decision: "deny", comment })}>
                  <Icon name="x" /> Deny
                </button>
              </div>
            </div>
          )}
          {r.canCancel && (
            <button className="btn ghost" disabled={busy} onClick={() => act("cancel")}>
              Cancel request
            </button>
          )}
          <ErrorNote error={actionError} />
          <h3>Timeline</h3>
          <ol className="timeline">
            {(r.events ?? []).map((e) => (
              <li key={e.id}>
                <div className="small">
                  <b>{e.actor}</b> {e.message}
                </div>
                <div className="small muted">{ago(e.at)}</div>
              </li>
            ))}
          </ol>
        </aside>
      </div>
    </div>
  );
}
