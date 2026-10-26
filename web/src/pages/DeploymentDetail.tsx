import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import type { ActionDef } from "@quai/core";
import { api, ApiError, ago, requestTitle, useApi, type DeploymentDetail as Detail, type PlanView, type RequestView } from "../api";
import { ChangesView, ErrorNote, Modal, RulesPreview, Sha, StatusBadge, SyncBadge } from "../components/Bits";
import { DynamicForm } from "../components/DynamicForm";
import { Icon } from "../components/Icon";

export function DeploymentDetail() {
  const { name } = useParams();
  const { data: d, error } = useApi<Detail>(`/deployments/${name}`, 4000);
  const [tab, setTab] = useState<"overview" | "files" | "history">("overview");
  const [action, setAction] = useState<ActionDef>();

  if (error) return <div className="page"><ErrorNote error={error} /></div>;
  if (!d) return <div className="page muted">Loading…</div>;
  const open = d.requests.find((r) => r.status === "pending" || r.status === "approved");

  return (
    <div className="page">
      <div className="crumb small muted">
        <Link to="/inventory">Inventory</Link> / {d.name}
      </div>
      <header className="page-head">
        <div>
          <h1 className="row">
            <span className="bp-icon">
              <Icon name={d.blueprint?.metadata.icon} size={20} />
            </span>
            {d.name} <SyncBadge sync={d.sync} />
          </h1>
          <p className="muted">
            {d.blueprint?.metadata.title ?? d.record.spec.blueprint} v{d.record.spec.blueprintVersion} · owned by <b>{d.record.metadata.owner}</b> · <code>{d.dir}/</code>
          </p>
        </div>
      </header>

      {open && (
        <div className="note warn">
          <Icon name="clock" size={14} /> <Link to={`/requests/${open.id}`}>{requestTitle(open)}</Link> is in progress; new changes are blocked until it completes.
        </div>
      )}
      {!d.blueprint && <div className="note warn">Blueprint {d.record.spec.blueprint} is no longer in the catalog; day-2 actions are unavailable.</div>}

      {d.canOperate && d.blueprint && (
        <div className="day2">
          {d.actions.map((a) => (
            <button key={a.name} className={`action ${a.name === "delete" ? "danger" : ""}`} disabled={!!open} onClick={() => setAction(a)} title={a.description}>
              <Icon name={a.icon} />
              <span>{a.title ?? a.name}</span>
            </button>
          ))}
        </div>
      )}

      <div className="tabs" role="tablist">
        {(["overview", "files", "history"] as const).map((t) => (
          <button key={t} role="tab" aria-selected={tab === t} className={tab === t ? "on" : ""} onClick={() => setTab(t)}>
            {t === "overview" ? "Overview" : t === "files" ? `Files (${d.files.length})` : `Git history (${d.history.length})`}
          </button>
        ))}
      </div>

      {tab === "overview" && (
        <div className="split">
          <div className="panel">
            <h2>Configuration</h2>
            <dl className="kv">
              {Object.entries(d.record.spec.inputs).map(([k, v]) => {
                const def = d.blueprint?.spec.inputs.find((i) => i.name === k);
                return (
                  <div key={k}>
                    <dt>{def?.title ?? k}</dt>
                    <dd>
                      <code>{JSON.stringify(v)}</code>
                    </dd>
                  </div>
                );
              })}
            </dl>
            <h2>Requests</h2>
            <table className="table compact">
              <tbody>
                {d.requests.map((r) => (
                  <tr key={r.id}>
                    <td className="muted">#{r.number}</td>
                    <td>
                      <Link to={`/requests/${r.id}`}>{requestTitle(r)}</Link>
                    </td>
                    <td>{r.requester}</td>
                    <td>
                      <StatusBadge status={r.status} />
                    </td>
                    <td className="small muted">{ago(r.createdAt)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <aside className="panel">
            <h2>Sync status</h2>
            {d.sync ? (
              <dl className="kv">
                <div>
                  <dt>Sync</dt>
                  <dd>{d.sync.sync}</dd>
                </div>
                <div>
                  <dt>Health</dt>
                  <dd>{d.sync.health}</dd>
                </div>
                <div>
                  <dt>Revision</dt>
                  <dd>
                    <Sha sha={d.sync.revision} />
                  </dd>
                </div>
                <div>
                  <dt>Reported</dt>
                  <dd>{ago(d.sync.at)}</dd>
                </div>
                {d.sync.message && (
                  <div>
                    <dt>Message</dt>
                    <dd className="small">{d.sync.message}</dd>
                  </div>
                )}
              </dl>
            ) : (
              <p className="muted small">No status reported yet. Point Argo CD notifications or Flux alerts at <code>/api/webhooks/sync</code>.</p>
            )}
            <h3>Activity</h3>
            <ol className="timeline">
              {d.events.map((e) => (
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
      )}
      {tab === "files" && (
        <div className="panel">
          <ChangesView changes={d.files.map((f) => ({ path: f.path, op: "add", after: f.content }))} collapsed />
        </div>
      )}
      {tab === "history" && (
        <div className="panel">
          <ol className="commits">
            {d.history.map((c) => (
              <li key={c.sha}>
                <Sha sha={c.sha} />
                <div>
                  <div className="strong">{c.subject}</div>
                  <div className="small muted">
                    {c.author} · {ago(c.date)}
                  </div>
                </div>
              </li>
            ))}
          </ol>
        </div>
      )}

      {action && <ActionModal deployment={d} action={action} onClose={() => setAction(undefined)} />}
    </div>
  );
}

function ActionModal({ deployment: d, action, onClose }: { deployment: Detail; action: ActionDef; onClose: () => void }) {
  const nav = useNavigate();
  const bp = d.blueprint!;
  const isUpdate = action.name === "update";
  const isDelete = action.name === "delete";
  const defs = isUpdate ? bp.spec.inputs : action.inputs ?? [];
  const [values, setValues] = useState<Record<string, unknown>>(() => {
    if (isUpdate) return { ...d.record.spec.inputs };
    // Prefill action inputs with the deployment's current values of the same name.
    return Object.fromEntries(defs.map((i) => [i.name, d.record.spec.inputs[i.name] ?? i.default]).filter(([, v]) => v !== undefined));
  });
  const [reason, setReason] = useState("");
  const [plan, setPlan] = useState<PlanView>();
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [err, setErr] = useState<Error>();
  const [confirm, setConfirm] = useState("");
  const base = `/deployments/${d.name}/actions/${action.name}`;

  useEffect(() => {
    const t = setTimeout(async () => {
      try {
        setPlan(await api<PlanView>(`${base}/plan`, { body: { inputs: values } }));
        setFieldErrors({});
        setErr(undefined);
      } catch (e) {
        setPlan(undefined);
        if (e instanceof ApiError && Object.keys(e.fields).length) setFieldErrors(e.fields);
        else setErr(e as Error);
      }
    }, 250);
    return () => clearTimeout(t);
  }, [values, base]);

  const submit = async () => {
    try {
      const r = await api<RequestView>(base, { body: { inputs: values, reason } });
      nav(`/requests/${r.id}`);
    } catch (e) {
      if (e instanceof ApiError) setFieldErrors(e.fields);
      setErr(e as Error);
    }
  };

  const immutable = isUpdate ? bp.spec.inputs.filter((i) => i.immutable).map((i) => i.name) : [];
  const blocked = !plan || (isDelete && confirm !== d.name);

  return (
    <Modal title={`${action.title ?? action.name} · ${d.name}`} onClose={onClose} wide>
      <div className="modal-body split">
        <div>
          {action.description && <p className="muted">{action.description}</p>}
          {isDelete ? (
            <div className="field">
              <label htmlFor="confirm">
                Type <b>{d.name}</b> to confirm
              </label>
              <input id="confirm" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="off" />
            </div>
          ) : (
            <DynamicForm inputs={defs} values={values} onChange={setValues} errors={fieldErrors} scopeKey={isUpdate ? "inputs" : "action"} extraScope={{ inputs: d.record.spec.inputs }} locked={immutable} />
          )}
          <div className="form">
            <div className={`field ${fieldErrors.reason ? "has-error" : ""}`}>
              <label htmlFor="action-reason">Reason</label>
              <textarea id="action-reason" rows={2} value={reason} onChange={(e) => setReason(e.target.value)} />
              {fieldErrors.reason && <div className="error">{fieldErrors.reason}</div>}
            </div>
          </div>
          <ErrorNote error={err} />
        </div>
        <div>
          <h3>Approvals</h3>
          {plan ? <RulesPreview rules={plan.rules} /> : <div className="muted small">—</div>}
          <h3>Diff</h3>
          {plan ? <ChangesView changes={plan.changes} /> : <div className="muted small">Adjust the form to see the change.</div>}
        </div>
      </div>
      <div className="modal-foot">
        <button className="btn ghost" onClick={onClose}>
          Cancel
        </button>
        <button className={`btn ${isDelete ? "danger" : "primary"}`} disabled={blocked} onClick={submit}>
          <Icon name={plan?.rules.length ? "shield" : "git"} /> {plan?.rules.length ? "Submit for approval" : "Submit and commit"}
        </button>
      </div>
    </Modal>
  );
}
