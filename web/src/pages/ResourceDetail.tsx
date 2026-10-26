import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { getPath, type ActionDef } from "@quai/core";
import { api, ApiError, ago, requestTitle, useApi, type PlanView, type RequestView, type ResourceDetail as Detail } from "../api";
import { ChangesView, ErrorNote, Modal, RulesPreview, Sha, StatusBadge, SyncBadge } from "../components/Bits";
import { DynamicForm } from "../components/DynamicForm";
import { SchemaForm } from "../components/SchemaForm";
import { Icon } from "../components/Icon";
import { managedFields, splitErrors } from "./NewTypedRequest";

export function ResourceDetail() {
  const { type, key } = useParams();
  const { data: d, error } = useApi<Detail>(`/types/${type}/resources/${encodeURIComponent(key ?? "")}`, 4000);
  const [tab, setTab] = useState<"overview" | "file" | "history">("overview");
  const [action, setAction] = useState<ActionDef>();

  if (error) return <div className="page"><ErrorNote error={error} /></div>;
  if (!d) return <div className="page muted">Loading…</div>;
  const open = d.requests.find((r) => r.status === "pending" || r.status === "approved");
  const name = d.params.name ?? d.key;

  return (
    <div className="page">
      <div className="crumb small muted">
        <Link to="/inventory">Inventory</Link> / {d.type.metadata.title} / {d.key}
      </div>
      <header className="page-head">
        <div>
          <h1 className="row">
            <span className="bp-icon">
              <Icon name={d.type.metadata.icon} size={20} />
            </span>
            {name} {d.params.env && <span className={`env ${d.params.env}`}>{d.params.env}</span>} <SyncBadge sync={d.sync} fallback="In repo" />
          </h1>
          <p className="muted">
            {d.type.metadata.title} · {d.owner ? (
              <>
                owned by <b>{d.owner}</b>
              </>
            ) : (
              <span className="tag warn">no owner on record</span>
            )}{" "}
            · <code>{d.path}</code>
            {d.stack && (
              <>
                {" "}
                · stack <code>{d.stack}</code>
              </>
            )}
          </p>
        </div>
      </header>

      {d.error && (
        <div className="note bad">
          <Icon name="alert" size={14} /> This file cannot be parsed, so Quai will not edit it: {d.error}
        </div>
      )}
      {open && (
        <div className="note warn">
          <Icon name="clock" size={14} /> <Link to={`/requests/${open.id}`}>{requestTitle(open)}</Link> is in progress; new changes are blocked until it completes.
        </div>
      )}
      {!d.canOperate && !d.error && (
        <div className="note info small">
          <Icon name="shield" size={14} /> {d.owner ? `Only ${d.owner} or a platform admin can change this resource.` : "Only platform admins can change resources without an owner."}
        </div>
      )}

      {d.canOperate && !d.error && (
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
        {(["overview", "file", "history"] as const).map((t) => (
          <button key={t} role="tab" aria-selected={tab === t} className={tab === t ? "on" : ""} onClick={() => setTab(t)}>
            {t === "overview" ? "Overview" : t === "file" ? "File" : `Git history (${d.history.length})`}
          </button>
        ))}
      </div>

      {tab === "overview" && (
        <div className="split">
          <div className="panel">
            <h2>Configuration</h2>
            {d.document && <DocView value={d.document} />}
            <h2>Requests</h2>
            {d.requests.length ? (
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
            ) : (
              <p className="muted small">No requests through Quai yet. This resource was found in the repository.</p>
            )}
          </div>
          <aside className="panel">
            <h2>Status</h2>
            {d.sync ? (
              <dl className="kv">
                <div>
                  <dt>State</dt>
                  <dd>
                    <SyncBadge sync={d.sync} />
                  </dd>
                </div>
                <div>
                  <dt>Revision</dt>
                  <dd>
                    <Sha sha={d.sync.revision} />
                  </dd>
                </div>
                {d.sync.message && (
                  <div>
                    <dt>Message</dt>
                    <dd className="small">{d.sync.message}</dd>
                  </div>
                )}
              </dl>
            ) : (
              <p className="muted small">No apply reported yet. Your pipeline can post results to <code>/api/webhooks/sync</code> with <code>deployment: {d.id}</code>.</p>
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
      {tab === "file" && (
        <div className="panel">
          <ChangesView changes={[{ path: d.path, op: "add", after: d.raw }]} />
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

      {action && <TypedActionModal resource={d} action={action} onClose={() => setAction(undefined)} />}
    </div>
  );
}

function DocView({ value }: { value: Record<string, unknown> }) {
  return (
    <dl className="kv">
      {Object.entries(value).map(([k, v]) => (
        <div key={k}>
          <dt>{k}</dt>
          <dd>
            <code>{typeof v === "object" ? JSON.stringify(v) : String(v)}</code>
          </dd>
        </div>
      ))}
    </dl>
  );
}

/**
 * Prefill action inputs from the resource: an input `x` used as `set: { a.b: ${{ action.x }} }`
 * starts at the current value of `a.b`.
 */
function prefill(action: ActionDef, doc: Record<string, unknown>) {
  const out: Record<string, unknown> = {};
  for (const [path, expr] of Object.entries(action.set ?? {})) {
    const m = typeof expr === "string" ? /^\s*\$\{\{\s*action\.([A-Za-z0-9_]+)\s*\}\}\s*$/.exec(expr) : null;
    const current = getPath(doc, path);
    if (m && current !== undefined) out[m[1]] = current;
  }
  for (const i of action.inputs ?? []) if (!(i.name in out) && i.default !== undefined) out[i.name] = i.default;
  return out;
}

function TypedActionModal({ resource: d, action, onClose }: { resource: Detail; action: ActionDef; onClose: () => void }) {
  const nav = useNavigate();
  const isUpdate = action.name === "update";
  const isDelete = action.name === "delete";
  const [doc, setDoc] = useState<Record<string, unknown>>(() => structuredClone(d.document ?? {}));
  const [inputs, setInputs] = useState<Record<string, unknown>>(() => prefill(action, d.document ?? {}));
  const [reason, setReason] = useState("");
  const [plan, setPlan] = useState<PlanView>();
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [err, setErr] = useState<Error>();
  const [confirm, setConfirm] = useState("");
  const base = `/types/${d.type.metadata.name}/resources/${encodeURIComponent(d.key)}/actions/${action.name}`;
  const payload = isUpdate ? { document: doc } : { inputs };

  useEffect(() => {
    const t = setTimeout(async () => {
      try {
        setPlan(await api<PlanView>(`${base}/plan`, { body: payload }));
        setErrors({});
        setErr(undefined);
      } catch (e) {
        setPlan(undefined);
        if (e instanceof ApiError && Object.keys(e.fields).length) {
          setErrors(splitErrors(e.fields).doc);
          setErr(undefined);
        } else setErr(e as Error);
      }
    }, 250);
    return () => clearTimeout(t);
  }, [doc, inputs, base]); // eslint-disable-line react-hooks/exhaustive-deps

  const submit = async () => {
    try {
      const r = await api<RequestView>(base, { body: { ...payload, reason } });
      nav(`/requests/${r.id}`);
    } catch (e) {
      if (e instanceof ApiError) setErrors(splitErrors(e.fields).doc);
      setErr(e as Error);
    }
  };

  return (
    <Modal title={`${action.title ?? action.name} · ${d.key}`} onClose={onClose} wide>
      <div className="modal-body split">
        <div>
          {action.description && <p className="muted">{action.description}</p>}
          {isDelete ? (
            <>
              <div className="note bad small">
                <Icon name="alert" size={14} /> Removes <code>{d.path}</code>. The next apply of <code>{d.stack ?? "its stack"}</code> destroys the resource.
              </div>
              <div className="field">
                <label htmlFor="confirm">
                  Type <b>{d.key}</b> to confirm
                </label>
                <input id="confirm" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="off" />
              </div>
            </>
          ) : isUpdate ? (
            <SchemaForm schema={d.type.resolvedSchema} value={doc} onChange={setDoc} errors={errors} ui={d.type.spec.ui} locked={d.type.locked} hidden={managedFields(d.type)} params={d.params} />
          ) : (
            <DynamicForm inputs={action.inputs ?? []} values={inputs} onChange={setInputs} errors={errors} scopeKey="action" extraScope={{ inputs: d.document ?? {}, params: d.params }} />
          )}
          <div className="form">
            <div className={`field ${errors.reason ? "has-error" : ""}`}>
              <label htmlFor="action-reason">Reason</label>
              <textarea id="action-reason" rows={2} value={reason} onChange={(e) => setReason(e.target.value)} />
              {errors.reason && <div className="error">{errors.reason}</div>}
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
        <button className={`btn ${isDelete ? "danger" : "primary"}`} disabled={!plan || (isDelete && confirm !== d.key)} onClick={submit}>
          <Icon name={plan?.rules.length ? "shield" : "git"} /> {plan?.rules.length ? "Submit for approval" : "Submit and commit"}
        </button>
      </div>
    </Modal>
  );
}
