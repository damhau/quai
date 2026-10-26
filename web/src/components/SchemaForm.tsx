import { deref, getPath, schemaDefaults, setPath, test, type FieldUi, type JsonSchema } from "@quai/core";
import { Icon } from "./Icon";

// Renders a form straight from the repository's JSON Schema, so the form and
// the CI validation can never disagree. Layout hints (order, visibility,
// immutability) come from the resource type, keeping the schema itself pure.

export interface SchemaFormProps {
  schema: JsonSchema;
  value: Record<string, unknown>;
  onChange: (v: Record<string, unknown>) => void;
  errors?: Record<string, string>;
  ui?: { order?: string[]; fields?: Record<string, FieldUi> };
  /** Shown but not editable (immutable on day-2). */
  locked?: string[];
  /** Not shown at all (Quai-managed fields). */
  hidden?: string[];
  params?: Record<string, unknown>;
}

interface Ctx extends Required<Omit<SchemaFormProps, "onChange" | "params">> {
  root: JsonSchema;
  set: (path: string, v: unknown) => void;
  params: Record<string, unknown>;
}

const typeOf = (s: JsonSchema): string | undefined => (Array.isArray(s.type) ? s.type.find((t: string) => t !== "null") : s.type) ?? (s.enum ? typeof s.enum[0] : s.properties ? "object" : undefined);
const join = (base: string, k: string | number) => (base ? `${base}.${k}` : String(k));

export function SchemaForm(props: SchemaFormProps) {
  const root = props.schema;
  const ctx: Ctx = {
    schema: root,
    root,
    value: props.value,
    errors: props.errors ?? {},
    ui: props.ui ?? {},
    locked: props.locked ?? [],
    hidden: props.hidden ?? [],
    params: props.params ?? {},
    set: (path, v) => {
      const next = structuredClone(props.value);
      setPath(next, path, v);
      props.onChange(next);
    },
  } as Ctx;
  const rootErr = ctx.errors.$;
  return (
    <div className="form schema-form">
      {rootErr && <div className="error">{rootErr}</div>}
      <ObjectFields schema={deref(root, root)} path="" ctx={ctx} />
    </div>
  );
}

function ObjectFields({ schema, path, ctx }: { schema: JsonSchema; path: string; ctx: Ctx }) {
  const props = Object.keys(schema.properties ?? {});
  const order = path === "" ? ctx.ui.order ?? [] : [];
  const keys = [...order.filter((k) => props.includes(k)), ...props.filter((k) => !order.includes(k))];
  const required: string[] = schema.required ?? [];
  return (
    <>
      {keys.map((k) => {
        const p = join(path, k);
        const ui = ctx.ui.fields?.[p] ?? {};
        if (ctx.hidden.includes(p) || ui.hidden) return null;
        if (ui.visibleIf && !test(ui.visibleIf, { inputs: ctx.value, params: ctx.params })) return null;
        return <Field key={p} schema={deref(schema.properties[k], ctx.root)} path={p} required={required.includes(k)} ctx={ctx} ui={ui} label={k} />;
      })}
    </>
  );
}

function Field({ schema, path, required, ctx, ui, label }: { schema: JsonSchema; path: string; required: boolean; ctx: Ctx; ui: FieldUi; label: string }) {
  const title = ui.title ?? schema.title ?? label;
  const description = ui.description ?? schema.description;
  const err = ctx.errors[path];
  const disabled = ctx.locked.includes(path);
  const t = typeOf(schema);
  const id = `sf-${path.replace(/\./g, "-")}`;

  if (t === "object" && schema.properties) {
    return (
      <fieldset className={`group ${err ? "has-error" : ""}`}>
        <legend>
          {title}
          {required && <span className="req">*</span>}
          {disabled && <span className="tag">immutable</span>}
        </legend>
        {description && <div className="desc">{description}</div>}
        {err && <div className="error">{err}</div>}
        <ObjectFields schema={schema} path={path} ctx={disabled ? { ...ctx, locked: [...ctx.locked, ...Object.keys(schema.properties).map((k) => join(path, k))] } : ctx} />
      </fieldset>
    );
  }

  return (
    <div className={`field ${err ? "has-error" : ""}`}>
      <label htmlFor={id} id={`${id}-l`}>
        {title}
        {required && <span className="req">*</span>}
        {disabled && <span className="tag">immutable</span>}
      </label>
      <Control schema={schema} path={path} id={id} ctx={ctx} disabled={disabled} />
      {description && <div className="desc">{description}</div>}
      {err && <div className="error">{err}</div>}
    </div>
  );
}

function Control({ schema, path, id, ctx, disabled }: { schema: JsonSchema; path: string; id: string; ctx: Ctx; disabled: boolean }) {
  const v = getPath(ctx.value, path);
  const t = typeOf(schema);

  if (schema.enum) {
    const options: unknown[] = schema.enum;
    const short = options.length <= 4 && options.every((o) => String(o).length <= 16);
    if (short)
      return (
        <div className="segmented" role="radiogroup" aria-labelledby={`${id}-l`}>
          {options.map((o) => (
            <button type="button" key={String(o)} role="radio" aria-checked={v === o} className={v === o ? "on" : ""} disabled={disabled} onClick={() => ctx.set(path, o)}>
              {String(o)}
            </button>
          ))}
        </div>
      );
    return (
      <select id={id} value={v === undefined ? "" : String(v)} disabled={disabled} onChange={(e) => ctx.set(path, e.target.value === "" ? undefined : options.find((o) => String(o) === e.target.value))}>
        <option value="">—</option>
        {options.map((o) => (
          <option key={String(o)} value={String(o)}>
            {String(o)}
          </option>
        ))}
      </select>
    );
  }
  if (t === "boolean")
    return (
      <label className="switch">
        <input id={id} type="checkbox" checked={v === true} disabled={disabled} onChange={(e) => ctx.set(path, e.target.checked)} />
        <span className="track" />
        <span>{v ? "Yes" : "No"}</span>
      </label>
    );
  if (t === "integer" || t === "number")
    return (
      <div className="number">
        <input
          id={id}
          type="number"
          value={v === undefined || v === null ? "" : String(v)}
          min={schema.minimum}
          max={schema.maximum}
          step={t === "integer" ? 1 : "any"}
          disabled={disabled}
          onChange={(e) => ctx.set(path, e.target.value === "" ? undefined : Number(e.target.value))}
        />
        {schema.minimum !== undefined && schema.maximum !== undefined && (
          <span className="hint">
            {schema.minimum}–{schema.maximum}
          </span>
        )}
      </div>
    );
  if (t === "array") return <ArrayControl schema={schema} path={path} ctx={ctx} disabled={disabled} />;
  if (t === "object") return <MapControl schema={schema} path={path} ctx={ctx} disabled={disabled} />;
  return (
    <input
      id={id}
      type="text"
      value={v === undefined || v === null ? "" : String(v)}
      disabled={disabled}
      maxLength={schema.maxLength}
      placeholder={schema.format ? schema.format : undefined}
      onChange={(e) => ctx.set(path, e.target.value === "" ? undefined : e.target.value)}
      autoComplete="off"
      spellCheck={false}
    />
  );
}

function ArrayControl({ schema, path, ctx, disabled }: { schema: JsonSchema; path: string; ctx: Ctx; disabled: boolean }) {
  const items = (getPath(ctx.value, path) as unknown[] | undefined) ?? [];
  const itemSchema = deref(schema.items ?? {}, ctx.root);
  const isObject = typeOf(itemSchema) === "object" && itemSchema.properties;
  const canAdd = !disabled && (schema.maxItems === undefined || items.length < schema.maxItems);
  const canRemove = !disabled && items.length > (schema.minItems ?? 0);
  const add = () => ctx.set(path, [...items, schemaDefaults(itemSchema, ctx.root) ?? (isObject ? {} : "")]);
  const remove = (i: number) => ctx.set(path, items.filter((_, j) => j !== i));
  return (
    <div className="array">
      {items.map((_, i) => (
        <div className={`array-item ${isObject ? "obj" : ""}`} key={i}>
          {isObject ? (
            <div className="array-fields">
              <ObjectFields schema={itemSchema} path={join(path, i)} ctx={ctx} />
            </div>
          ) : (
            <Control schema={itemSchema} path={join(path, i)} id={`sf-${path}-${i}`} ctx={ctx} disabled={disabled} />
          )}
          <button type="button" className="icon-btn tiny" disabled={!canRemove} onClick={() => remove(i)} aria-label={`Remove item ${i + 1}`}>
            <Icon name="trash" size={12} />
          </button>
        </div>
      ))}
      <button type="button" className="btn sm ghost" disabled={!canAdd} onClick={add}>
        <Icon name="plus" size={14} /> Add
      </button>
    </div>
  );
}

/** `additionalProperties` maps such as tags: key/value rows. */
function MapControl({ schema, path, ctx, disabled }: { schema: JsonSchema; path: string; ctx: Ctx; disabled: boolean }) {
  const map = (getPath(ctx.value, path) as Record<string, unknown> | undefined) ?? {};
  const valueSchema = typeof schema.additionalProperties === "object" ? deref(schema.additionalProperties, ctx.root) : { type: "string" };
  const numeric = ["integer", "number"].includes(typeOf(valueSchema) ?? "");
  const entries = Object.entries(map);
  const write = (rows: [string, unknown][]) => ctx.set(path, rows.length ? Object.fromEntries(rows) : undefined);
  return (
    <div className="kv-edit">
      {entries.map(([k, val], i) => (
        <div className="row" key={i}>
          <input value={k} disabled={disabled} aria-label="Key" onChange={(e) => write(entries.map((x, j) => (j === i ? [e.target.value, x[1]] : x)))} />
          <input
            value={val === undefined ? "" : String(val)}
            disabled={disabled}
            aria-label={`Value for ${k}`}
            type={numeric ? "number" : "text"}
            onChange={(e) => write(entries.map((x, j) => (j === i ? [x[0], numeric ? Number(e.target.value) : e.target.value] : x)))}
          />
          <button type="button" className="icon-btn tiny" disabled={disabled} onClick={() => write(entries.filter((_, j) => j !== i))} aria-label={`Remove ${k}`}>
            <Icon name="trash" size={12} />
          </button>
        </div>
      ))}
      <button type="button" className="btn sm ghost" disabled={disabled || "" in map} onClick={() => write([...entries, ["", numeric ? 0 : ""]])}>
        <Icon name="plus" size={14} /> Add entry
      </button>
    </div>
  );
}
