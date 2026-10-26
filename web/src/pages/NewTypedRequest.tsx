import { useEffect, useMemo, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { defaultsFor, schemaDefaults } from "@quai/core";
import { api, ApiError, useApi, type PlanView, type RequestView, type TypeView } from "../api";
import { DynamicForm } from "../components/DynamicForm";
import { SchemaForm } from "../components/SchemaForm";
import { ChangesView, ErrorNote, RulesPreview } from "../components/Bits";
import { Icon } from "../components/Icon";

/** Errors for path params come back as `@param`; everything else is a document field. */
export function splitErrors(fields: Record<string, string>) {
  const params: Record<string, string> = {};
  const doc: Record<string, string> = {};
  for (const [k, v] of Object.entries(fields)) (k.startsWith("@") ? params : doc)[k.replace(/^@/, "")] = v;
  return { params, doc };
}

export const managedFields = (t: TypeView) => [...Object.values(t.spec.paramFields ?? {}), ...(t.spec.ownerField ? [t.spec.ownerField] : [])];

export function NewTypedRequest() {
  const { type: name } = useParams();
  const nav = useNavigate();
  const { data: t, error } = useApi<TypeView>(`/types/${name}`);
  const [params, setParams] = useState<Record<string, unknown>>({});
  const [doc, setDoc] = useState<Record<string, unknown>>({});
  const [reason, setReason] = useState("");
  const [plan, setPlan] = useState<PlanView>();
  const [errors, setErrors] = useState<{ params: Record<string, string>; doc: Record<string, string> }>({ params: {}, doc: {} });
  const [planError, setPlanError] = useState<Error>();
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    if (!t) return;
    setParams(defaultsFor(t.paramInputs));
    setDoc((schemaDefaults(t.resolvedSchema) as Record<string, unknown>) ?? {});
  }, [t]);

  const body = useMemo(() => ({ params, document: doc }), [params, doc]);
  const lastParam = t?.paramInputs[t.paramInputs.length - 1]?.name;
  const ready = !!t && !!lastParam && !!params[lastParam];

  useEffect(() => {
    if (!ready) {
      setPlan(undefined);
      return;
    }
    const timer = setTimeout(async () => {
      try {
        setPlan(await api<PlanView>(`/types/${name}/plan`, { body }));
        setErrors({ params: {}, doc: {} });
        setPlanError(undefined);
      } catch (e) {
        setPlan(undefined);
        if (e instanceof ApiError && Object.keys(e.fields).length) {
          setErrors(splitErrors(e.fields));
          setPlanError(undefined);
        } else setPlanError(e as Error);
      }
    }, 300);
    return () => clearTimeout(timer);
  }, [body, ready, name]);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setSubmitting(true);
    try {
      const r = await api<RequestView>(`/types/${name}/requests`, { body: { ...body, reason } });
      nav(`/requests/${r.id}`);
    } catch (err) {
      if (err instanceof ApiError) setErrors(splitErrors(err.fields));
      setPlanError(err as Error);
    } finally {
      setSubmitting(false);
    }
  };

  if (error) return <div className="page"><ErrorNote error={error} /></div>;
  if (!t) return <div className="page muted">Loading…</div>;
  const needsApproval = (plan?.rules.length ?? 0) > 0;

  return (
    <div className="page split">
      <form className="panel" onSubmit={submit}>
        <div className="crumb small muted">Catalog / {t.metadata.title}</div>
        <h1 className="row">
          <span className="bp-icon">
            <Icon name={t.metadata.icon} size={20} />
          </span>
          {t.metadata.title}
        </h1>
        <p className="muted">{t.metadata.description}</p>
        <div className="note info small">
          <Icon name="file" size={14} /> Written to <code>{t.spec.locate.path}</code>
          {t.spec.stack && (
            <>
              {" "}
              · applied by stack <code>{t.spec.stack}</code>
            </>
          )}
        </div>
        <DynamicForm inputs={t.paramInputs} values={params} onChange={setParams} errors={errors.params} scopeKey="params" />
        <SchemaForm schema={t.resolvedSchema} value={doc} onChange={setDoc} errors={errors.doc} ui={t.spec.ui} hidden={managedFields(t)} params={params} />
        <div className="form">
          <div className={`field ${errors.doc.reason ? "has-error" : ""}`}>
            <label htmlFor="reason">Reason {needsApproval && <span className="req">*</span>}</label>
            <textarea id="reason" rows={3} value={reason} onChange={(e) => setReason(e.target.value)} placeholder={needsApproval ? "Approvers will see this" : "Optional — ends up in the commit message"} />
            {errors.doc.reason && <div className="error">{errors.doc.reason}</div>}
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
        {!ready && <div className="muted small">Fill in the {t.paramInputs.map((p) => p.title ?? p.name).join(" and ")} to see the commit preview.</div>}
        {ready && !plan && Object.keys(errors.doc).length + Object.keys(errors.params).length > 0 && (
          <div className="muted small">Fix the highlighted fields to see the preview.</div>
        )}
        {plan && (
          <>
            {plan.stack && (
              <div className="small muted">
                stack <code>{plan.stack}</code>
              </div>
            )}
            <h3>Approvals</h3>
            <RulesPreview rules={plan.rules} />
            <h3>Changes</h3>
            <ChangesView changes={plan.changes} />
          </>
        )}
      </aside>
    </div>
  );
}
