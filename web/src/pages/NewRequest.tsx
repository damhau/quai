import { useEffect, useMemo, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { defaultsFor, type Blueprint } from "@quai/core";
import { api, ApiError, useApi, type PlanView, type RequestView } from "../api";
import { DynamicForm } from "../components/DynamicForm";
import { ChangesView, ErrorNote, RulesPreview } from "../components/Bits";
import { Icon } from "../components/Icon";

export function NewRequest() {
  const { blueprint: name } = useParams();
  const nav = useNavigate();
  const { data: bp, error } = useApi<Blueprint>(`/blueprints/${name}`);
  const [depName, setDepName] = useState("");
  const [values, setValues] = useState<Record<string, unknown>>({});
  const [reason, setReason] = useState("");
  const [plan, setPlan] = useState<PlanView>();
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [planError, setPlanError] = useState<Error>();
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    if (bp) setValues(defaultsFor(bp.spec.inputs));
  }, [bp]);

  const body = useMemo(() => ({ blueprint: name, name: depName, inputs: values }), [name, depName, values]);

  // Live dry-run: the right-hand panel always shows the exact commit and approvers.
  useEffect(() => {
    if (!bp || !depName) {
      setPlan(undefined);
      return;
    }
    const t = setTimeout(async () => {
      try {
        setPlan(await api<PlanView>("/plan", { body }));
        setFieldErrors({});
        setPlanError(undefined);
      } catch (e) {
        setPlan(undefined);
        if (e instanceof ApiError && Object.keys(e.fields).length) {
          setFieldErrors(e.fields);
          setPlanError(undefined);
        } else setPlanError(e as Error);
      }
    }, 300);
    return () => clearTimeout(t);
  }, [body, bp]);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setSubmitting(true);
    try {
      const r = await api<RequestView>("/requests", { body: { ...body, reason } });
      nav(`/requests/${r.id}`);
    } catch (err) {
      if (err instanceof ApiError) setFieldErrors(err.fields);
      setPlanError(err as Error);
    } finally {
      setSubmitting(false);
    }
  };

  if (error) return <div className="page"><ErrorNote error={error} /></div>;
  if (!bp) return <div className="page muted">Loading…</div>;
  const needsApproval = (plan?.rules.length ?? 0) > 0;

  return (
    <div className="page split">
      <form className="panel" onSubmit={submit}>
        <div className="crumb small muted">Catalog / {bp.metadata.title}</div>
        <h1 className="row">
          <span className="bp-icon">
            <Icon name={bp.metadata.icon} size={20} />
          </span>
          {bp.metadata.title}
        </h1>
        <p className="muted">{bp.metadata.description}</p>
        <div className="form">
          <div className={`field ${fieldErrors.name ? "has-error" : ""}`}>
            <label htmlFor="dep-name">
              Deployment name<span className="req">*</span>
            </label>
            <input id="dep-name" value={depName} onChange={(e) => setDepName(e.target.value.toLowerCase())} placeholder="e.g. orders-db" autoFocus autoComplete="off" spellCheck={false} />
            <div className="desc">Becomes the Git directory and the Kubernetes resource names.</div>
            {fieldErrors.name && <div className="error">{fieldErrors.name}</div>}
          </div>
        </div>
        <DynamicForm inputs={bp.spec.inputs} values={values} onChange={setValues} errors={fieldErrors} />
        <div className="form">
          <div className={`field ${fieldErrors.reason ? "has-error" : ""}`}>
            <label htmlFor="reason">Reason {needsApproval && <span className="req">*</span>}</label>
            <textarea id="reason" rows={3} value={reason} onChange={(e) => setReason(e.target.value)} placeholder={needsApproval ? "Approvers will see this" : "Optional — ends up in the commit message"} />
            {fieldErrors.reason && <div className="error">{fieldErrors.reason}</div>}
          </div>
        </div>
        <ErrorNote error={planError} />
        <div className="actions">
          <button className="btn primary" disabled={!plan || submitting}>
            <Icon name={needsApproval ? "shield" : "git"} /> {needsApproval ? "Submit for approval" : "Submit and commit"}
          </button>
        </div>
      </form>
      <aside className="panel preview">
        <h2>
          <Icon name="git" /> What will be committed
        </h2>
        {!depName && <div className="muted small">Name your deployment to see the commit preview.</div>}
        {plan && (
          <>
            <div className="small muted">
              to <code>{plan.dir}/</code>
            </div>
            <h3>Approvals</h3>
            <RulesPreview rules={plan.rules} />
            <h3>Changes</h3>
            <ChangesView changes={plan.changes} collapsed />
          </>
        )}
      </aside>
    </div>
  );
}
