import { Plus, Trash2 } from "lucide-react"
import type { UiView } from "@/api/generated/types.gen"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Switch } from "@/components/ui/switch"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { cn } from "@/lib/utils"
import { defaults, deref, getPath, join, setPath, typeOf, type JsonSchema, type Values } from "./schema"

// Renders a form from the repository's JSON Schema, so that the form and the validation in CI
// cannot disagree. Layout hints (order, visibility, immutability) come from the definition's
// `ui`, keeping the schema itself plain.

export type SchemaFormProps = {
  schema: unknown
  /** Property names in the order the schema declares them (the API's propertyOrder). */
  propertyOrder?: string[] | null
  ui?: UiView
  value: Values
  onChange: (v: Values) => void
  /** Errors by field path; "$" is the document itself. */
  errors?: Record<string, string>
  /** Fields not shown: those Quai sets itself and those whose visibleIf is false. */
  hidden?: Set<string>
  /** Fields shown but not editable. */
  locked?: string[]
  /** Fields defined in code (an HCL expression), by path, with the expression: shown, never edited. */
  code?: Record<string, string>
  /** Called when a field loses focus, to show its errors from then on. */
  onTouch?: (path: string) => void
}

type Ctx = Required<Omit<SchemaFormProps, "schema" | "propertyOrder" | "ui" | "onChange">> & {
  root: unknown
  rank: Map<string, number>
  topOrder: string[]
  set: (path: string, v: unknown) => void
}

export function SchemaForm({ schema, propertyOrder, ui, value, onChange, errors, hidden, locked, code, onTouch }: SchemaFormProps) {
  const ctx: Ctx = {
    root: schema,
    rank: new Map((propertyOrder ?? []).map((k, i) => [k, i])),
    topOrder: ui?.order ?? [],
    value,
    errors: errors ?? {},
    hidden: hidden ?? new Set(),
    locked: locked ?? [],
    code: code ?? {},
    onTouch: onTouch ?? (() => {}),
    set: (path, v) => onChange(setPath(value, path, v)),
  }
  const rootError = ctx.errors.$
  return (
    <div className="grid gap-5">
      {rootError && <p className="text-destructive text-sm">{rootError}</p>}
      <ObjectFields schema={deref(schema as JsonSchema, schema)} path="" ctx={ctx} />
    </div>
  )
}

function orderKeys(keys: string[], ctx: Ctx, top: boolean) {
  const rank = (k: string) => {
    const i = top ? ctx.topOrder.indexOf(k) : -1
    return i >= 0 ? i - ctx.topOrder.length : (ctx.rank.get(k) ?? Number.MAX_SAFE_INTEGER)
  }
  return [...keys].sort((a, b) => rank(a) - rank(b) || a.localeCompare(b))
}

function ObjectFields({ schema, path, ctx }: { schema: JsonSchema; path: string; ctx: Ctx }) {
  const props = schema.properties ?? {}
  return (
    <>
      {orderKeys(Object.keys(props), ctx, path === "").map((k) => {
        const p = join(path, k)
        if (ctx.hidden.has(p)) return null
        return <Field key={p} schema={deref(props[k], ctx.root)} path={p} name={k} required={schema.required?.includes(k) ?? false} ctx={ctx} />
      })}
    </>
  )
}

const fieldId = (path: string) => `field-${path.replaceAll(".", "-")}`

function Field({ schema, path, name, required, ctx }: { schema: JsonSchema; path: string; name: string; required: boolean; ctx: Ctx }) {
  const title = schema.title ?? name
  const error = ctx.errors[path]
  const locked = ctx.locked.includes(path) || schema.readOnly === true
  const id = fieldId(path)
  const heading = (
    <>
      {title}
      {required && (
        <span className="text-destructive" aria-hidden>
          *
        </span>
      )}
      {locked && <Badge variant="outline">immutable</Badge>}
    </>
  )
  const help = (
    <>
      {schema.description && (
        <p id={`${id}-desc`} className="text-muted-foreground text-xs">
          {schema.description}
        </p>
      )}
      {error && (
        <p id={`${id}-error`} className="text-destructive text-xs">
          {error}
        </p>
      )}
    </>
  )

  // A value defined in code is not in the document: the form shows the expression and leaves it.
  const expression = ctx.code[path]
  if (expression !== undefined) {
    return (
      <div className="grid gap-2">
        <p className="flex items-center gap-1.5 text-sm font-medium">
          {title}
          <Badge variant="outline">defined in code</Badge>
        </p>
        <code className="bg-muted rounded px-2 py-1.5 font-mono text-xs break-all">{expression}</code>
        <p className="text-muted-foreground text-xs">An expression, not a value: Quai keeps it as it is. Change it in the repository.</p>
      </div>
    )
  }
  if (typeOf(schema) === "object" && schema.properties) {
    const inner = locked ? { ...ctx, locked: [...ctx.locked, ...Object.keys(schema.properties).map((k) => join(path, k))] } : ctx
    return (
      <fieldset className="grid gap-4 rounded-lg border p-4">
        <legend className="flex items-center gap-1.5 px-1 text-sm font-medium">{heading}</legend>
        {help}
        <ObjectFields schema={schema} path={path} ctx={inner} />
      </fieldset>
    )
  }
  return (
    <div className="grid gap-2" onBlur={() => ctx.onTouch(path)}>
      <Label htmlFor={id} id={`${id}-label`}>
        {heading}
      </Label>
      <Control schema={schema} path={path} id={id} title={title} required={required} disabled={locked} ctx={ctx} />
      {help}
    </div>
  )
}

type ControlProps = { schema: JsonSchema; path: string; id: string; title: string; required: boolean; disabled: boolean; ctx: Ctx }

function describedBy(id: string, schema: JsonSchema, error: string | undefined) {
  return [schema.description && `${id}-desc`, error && `${id}-error`].filter(Boolean).join(" ") || undefined
}

function Control(props: ControlProps) {
  const { schema, path, id, disabled, ctx } = props
  const v = getPath(ctx.value, path)
  const t = typeOf(schema)
  const error = ctx.errors[path]
  const aria = { "aria-invalid": error ? true : undefined, "aria-describedby": describedBy(id, schema, error) }

  if (schema.enum) return <EnumControl {...props} options={schema.enum} value={v} onChange={(o) => ctx.set(path, o)} />
  if (t === "boolean")
    return (
      <div className="flex items-center gap-2">
        <Switch id={id} checked={v === true} disabled={disabled} onCheckedChange={(c) => ctx.set(path, c)} {...aria} />
        <span className="text-muted-foreground text-sm">{v === true ? "Yes" : "No"}</span>
      </div>
    )
  if (t === "integer" || t === "number")
    return (
      <div className="flex items-center gap-3">
        <Input
          id={id}
          type="number"
          className="w-40"
          value={typeof v === "number" ? String(v) : ""}
          min={schema.minimum}
          max={schema.maximum}
          step={t === "integer" ? 1 : "any"}
          disabled={disabled}
          onChange={(e) => ctx.set(path, e.target.value === "" || Number.isNaN(e.target.valueAsNumber) ? undefined : e.target.valueAsNumber)}
          {...aria}
        />
        {schema.minimum !== undefined && schema.maximum !== undefined && (
          <span className="text-muted-foreground text-xs">
            {schema.minimum}–{schema.maximum}
          </span>
        )}
      </div>
    )
  if (t === "array") return <ArrayControl {...props} />
  if (t === "object") return <MapControl {...props} />
  if (t === "string" || t === undefined)
    return (
      <Input
        id={id}
        value={typeof v === "string" ? v : ""}
        maxLength={schema.maxLength}
        placeholder={schema.format}
        disabled={disabled}
        autoComplete="off"
        spellCheck={false}
        onChange={(e) => ctx.set(path, e.target.value === "" ? undefined : e.target.value)}
        {...aria}
      />
    )
  return <p className="text-muted-foreground text-sm">This field is edited in Git.</p>
}

/** EnumControl offers a few short options as a segmented control and longer lists as a select. */
export function EnumControl({
  id,
  title,
  required,
  disabled,
  options,
  value,
  onChange,
}: {
  id: string
  title: string
  required: boolean
  disabled: boolean
  options: unknown[]
  value: unknown
  onChange: (v: unknown) => void
}) {
  const find = (s: string) => options.find((o) => String(o) === s)
  if (options.length <= 4 && options.every((o) => String(o).length <= 16))
    return (
      <ToggleGroup
        id={id}
        type="single"
        variant="outline"
        aria-label={title}
        value={value === undefined ? "" : String(value)}
        disabled={disabled}
        onValueChange={(s) => s !== "" && onChange(find(s))}
        className="w-fit"
      >
        {options.map((o) => (
          <ToggleGroupItem key={String(o)} value={String(o)} className="px-3">
            {String(o)}
          </ToggleGroupItem>
        ))}
      </ToggleGroup>
    )
  return (
    <NativeSelect id={id} value={value === undefined ? "" : String(value)} disabled={disabled} onChange={(e) => onChange(e.target.value === "" ? undefined : find(e.target.value))}>
      {(!required || value === undefined) && <NativeSelectOption value="">—</NativeSelectOption>}
      {options.map((o) => (
        <NativeSelectOption key={String(o)} value={String(o)}>
          {String(o)}
        </NativeSelectOption>
      ))}
    </NativeSelect>
  )
}

function ArrayControl({ schema, path, title, disabled, ctx }: ControlProps) {
  const items = Array.isArray(getPath(ctx.value, path)) ? (getPath(ctx.value, path) as unknown[]) : []
  const item = deref(schema.items, ctx.root)
  const objects = typeOf(item) === "object" && !!item.properties
  const canAdd = !disabled && (schema.maxItems === undefined || items.length < schema.maxItems)
  const canRemove = !disabled && items.length > (schema.minItems ?? 0)
  const add = () => ctx.set(path, [...items, defaults(item, ctx.root) ?? (objects ? {} : undefined)])
  const remove = (i: number) => ctx.set(path, items.filter((_, j) => j !== i))
  return (
    <div className="grid gap-2">
      {items.map((_, i) => {
        const p = join(path, i)
        return (
          <div key={i} className={cn("flex items-start gap-2", objects && "rounded-lg border p-3")}>
            <div className={cn("grid flex-1 gap-3", objects && "sm:grid-cols-2")}>
              {objects ? (
                <ObjectFields schema={item} path={p} ctx={ctx} />
              ) : (
                <Control schema={item} path={p} id={fieldId(p)} title={`${title} ${i + 1}`} required disabled={disabled} ctx={ctx} />
              )}
            </div>
            <Button type="button" variant="ghost" size="icon" disabled={!canRemove} onClick={() => remove(i)} aria-label={`Remove ${title} ${i + 1}`}>
              <Trash2 />
            </Button>
          </div>
        )
      })}
      <Button type="button" variant="outline" size="sm" className="w-fit" disabled={!canAdd} onClick={add} aria-label={`Add to ${title}`}>
        <Plus /> Add
      </Button>
    </div>
  )
}

/** MapControl edits a map of names to values (additionalProperties), such as tags. */
function MapControl({ schema, path, title, disabled, ctx }: ControlProps) {
  const raw = getPath(ctx.value, path)
  const entries = Object.entries(raw !== null && typeof raw === "object" ? (raw as Values) : {})
  const valueSchema = typeof schema.additionalProperties === "object" ? deref(schema.additionalProperties, ctx.root) : { type: "string" }
  const numeric = ["integer", "number"].includes(typeOf(valueSchema) ?? "")
  const write = (rows: [string, unknown][]) => ctx.set(path, rows.length ? Object.fromEntries(rows) : undefined)
  return (
    <div className="grid gap-2">
      {entries.map(([k, v], i) => (
        <div key={i} className="flex items-center gap-2">
          <Input value={k} disabled={disabled} aria-label={`${title}: name ${i + 1}`} onChange={(e) => write(entries.map((x, j) => (j === i ? [e.target.value, x[1]] : x)))} />
          <Input
            value={v === undefined || v === null ? "" : String(v)}
            type={numeric ? "number" : "text"}
            disabled={disabled}
            aria-label={`${title}: value of ${k || `entry ${i + 1}`}`}
            onChange={(e) => write(entries.map((x, j) => (j === i ? [x[0], numeric ? e.target.valueAsNumber : e.target.value] : x)))}
          />
          <Button type="button" variant="ghost" size="icon" disabled={disabled} onClick={() => write(entries.filter((_, j) => j !== i))} aria-label={`Remove ${k || `entry ${i + 1}`}`}>
            <Trash2 />
          </Button>
        </div>
      ))}
      <Button type="button" variant="outline" size="sm" className="w-fit" disabled={disabled || entries.some(([k]) => k === "")} onClick={() => write([...entries, ["", numeric ? 0 : ""]])} aria-label={`Add to ${title}`}>
        <Plus /> Add entry
      </Button>
    </div>
  )
}
