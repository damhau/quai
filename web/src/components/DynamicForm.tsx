import { isVisible, type InputDef } from "@quai/core";

export function optionList(def: InputDef) {
  return (def.options ?? []).map((o) => (typeof o === "string" ? { value: o, label: o } : o));
}

/**
 * Renders a blueprint's inputs as a form. Visibility rules use the same
 * expression engine as the server, so what you see is what gets validated.
 */
export function DynamicForm({
  inputs,
  values,
  onChange,
  errors = {},
  scopeKey = "inputs",
  locked = [],
  extraScope,
}: {
  inputs: InputDef[];
  values: Record<string, unknown>;
  onChange: (v: Record<string, unknown>) => void;
  errors?: Record<string, string>;
  scopeKey?: string;
  locked?: string[];
  extraScope?: Record<string, unknown>;
}) {
  const set = (name: string, v: unknown) => onChange({ ...values, [name]: v });
  return (
    <div className="form">
      {inputs
        .filter((d) => isVisible(d, values, scopeKey, extraScope))
        .map((d) => {
          const id = `f-${d.name}`;
          const v = values[d.name];
          const disabled = locked.includes(d.name);
          const err = errors[d.name];
          let control: React.ReactNode;
          if (d.type === "boolean") {
            control = (
              <label className="switch">
                <input id={id} type="checkbox" checked={v === true} disabled={disabled} onChange={(e) => set(d.name, e.target.checked)} />
                <span className="track" />
                <span>{v ? "Yes" : "No"}</span>
              </label>
            );
          } else if (d.type === "enum") {
            const opts = optionList(d);
            control =
              opts.length <= 4 ? (
                <div className="segmented" role="radiogroup" aria-labelledby={`${id}-l`}>
                  {opts.map((o) => (
                    <button type="button" key={o.value} role="radio" aria-checked={v === o.value} className={v === o.value ? "on" : ""} disabled={disabled} onClick={() => set(d.name, o.value)}>
                      {o.label}
                    </button>
                  ))}
                </div>
              ) : (
                <select id={id} value={String(v ?? "")} disabled={disabled} onChange={(e) => set(d.name, e.target.value)}>
                  {!d.required && <option value="">—</option>}
                  {opts.map((o) => (
                    <option key={o.value} value={o.value}>
                      {o.label}
                    </option>
                  ))}
                </select>
              );
          } else if (d.type === "integer" || d.type === "number") {
            control = (
              <div className="number">
                <input
                  id={id}
                  type="number"
                  value={v === undefined || v === null ? "" : String(v)}
                  min={d.min}
                  max={d.max}
                  step={d.type === "integer" ? 1 : "any"}
                  disabled={disabled}
                  onChange={(e) => set(d.name, e.target.value === "" ? undefined : Number(e.target.value))}
                />
                {d.min !== undefined && d.max !== undefined && (
                  <span className="hint">
                    {d.min}–{d.max}
                  </span>
                )}
              </div>
            );
          } else {
            control = (
              <input id={id} type="text" value={String(v ?? "")} placeholder={d.placeholder} disabled={disabled} onChange={(e) => set(d.name, e.target.value)} autoComplete="off" spellCheck={false} />
            );
          }
          return (
            <div className={`field ${err ? "has-error" : ""}`} key={d.name}>
              <label id={`${id}-l`} htmlFor={id}>
                {d.title ?? d.name}
                {d.required && <span className="req">*</span>}
                {disabled && <span className="tag">immutable</span>}
              </label>
              {control}
              {d.description && <div className="desc">{d.description}</div>}
              {err && <div className="error">{err}</div>}
            </div>
          );
        })}
    </div>
  );
}
