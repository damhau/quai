import Ajv, { type ErrorObject, type ValidateFunction } from "ajv";
import Ajv2020 from "ajv/dist/2020";
import addFormats from "ajv-formats";
import { parse as parseYaml, stringify } from "yaml";
import { parse as parseExpr, test, type Scope } from "./expr.ts";
import type { ActionDef, ApprovalPolicy, InputDef } from "./types.ts";

// A resource type maps one kind of thing (a VM, a DNS record, a namespace
// quota) onto an existing IaC repository. Quai never imposes a layout: the type
// says where instances live (`locate`), what a valid instance looks like (a
// JSON Schema that CI can validate with too) and who must approve changes.
// The inventory is whatever the repository contains, including resources
// that were written by hand long before Quai existed.

export interface ParamDef {
  name: string;
  title?: string;
  description?: string;
  type?: "string" | "enum";
  options?: string[];
  /** Offer the values that already exist in the repository at this position. */
  discover?: boolean;
  pattern?: string;
  default?: string;
}

export interface FieldUi {
  title?: string;
  description?: string;
  /** Expression over `inputs` (the document) and `params`; hidden fields are dropped. */
  visibleIf?: string;
  /** Can be set on create but not changed afterwards. */
  immutable?: boolean;
  hidden?: boolean;
}

export interface ResourceType {
  apiVersion: "quai.dev/v1alpha1";
  kind: "ResourceType";
  metadata: {
    name: string;
    title: string;
    description?: string;
    icon?: string;
    category?: string;
    version?: string;
    tags?: string[];
  };
  spec: {
    locate: {
      /** `file`: one file per resource. (Entries in shared files come later.) */
      strategy: "file";
      /** Path pattern with `{param}` placeholders, e.g. `envs/{env}/vms/{name}.yaml`. */
      path: string;
    };
    params?: ParamDef[];
    /** Repository path of a JSON Schema for the file content, or the schema inline. */
    schema: string | Record<string, unknown>;
    /** Document fields that mirror a path param (e.g. `name: name`). */
    paramFields?: Record<string, string>;
    /** Dotted path of the field holding the owner; Quai sets it on create. */
    ownerField?: string;
    /** Principals allowed to change any resource of this type (admins always can). */
    operators?: string[];
    /** Where plan/apply runs for this resource, e.g. `envs/{env}`. */
    stack?: string;
    ui?: { order?: string[]; fields?: Record<string, FieldUi> };
    approval?: ApprovalPolicy;
    /** Day-2 actions. `set` keys are dotted document paths (`disks.0.size_gb`). */
    actions?: ActionDef[];
  };
}

// ------------------------------------------------------------- path patterns

const SEGMENT_VALUE = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$/;
const PARAM = /\{([A-Za-z_][A-Za-z0-9_]*)\}/g;

export interface PathPattern {
  source: string;
  params: string[];
  /** Fixed leading directories, where scanning starts. */
  prefix: string;
  regex: RegExp;
}

const escapeRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

export function compilePattern(source: string): PathPattern {
  const params: string[] = [];
  let regex = "^";
  let last = 0;
  for (const m of source.matchAll(PARAM)) {
    regex += escapeRe(source.slice(last, m.index)) + "([^/]+)";
    params.push(m[1]);
    last = m.index! + m[0].length;
  }
  regex += escapeRe(source.slice(last)) + "$";
  const firstParam = source.indexOf("{");
  const prefix = firstParam < 0 ? source : source.slice(0, source.lastIndexOf("/", firstParam) + 1);
  return { source, params, prefix, regex: new RegExp(regex) };
}

export function matchPattern(p: PathPattern, path: string): Record<string, string> | undefined {
  const m = p.regex.exec(path);
  if (!m) return undefined;
  const out: Record<string, string> = {};
  p.params.forEach((name, i) => (out[name] = m[i + 1]));
  return Object.values(out).every(safeSegment) ? out : undefined;
}

export const safeSegment = (v: string) => SEGMENT_VALUE.test(v) && !v.includes("..");

export function formatPattern(p: PathPattern | string, params: Record<string, unknown>): string {
  const source = typeof p === "string" ? p : p.source;
  return source.replace(PARAM, (_, name: string) => {
    const v = String(params[name] ?? "");
    if (!safeSegment(v)) throw new Error(`Invalid value for {${name}}: '${v}'`);
    return v;
  });
}

/** Identifier of a resource inside its type: param values in pattern order. */
export const resourceKey = (p: PathPattern, params: Record<string, string>) => p.params.map((n) => params[n]).join("/");

export function paramDefs(t: ResourceType): ParamDef[] {
  const declared = new Map((t.spec.params ?? []).map((d) => [d.name, d]));
  return compilePattern(t.spec.locate.path).params.map((name) => declared.get(name) ?? { name, title: name });
}

/** Param definitions as form inputs. Discovered options are passed in by the caller. */
export function paramInputs(t: ResourceType, discovered: Record<string, string[]> = {}): InputDef[] {
  return paramDefs(t).map((d) => {
    const options = d.options ?? (d.discover ? discovered[d.name] ?? [] : undefined);
    return options
      ? { name: d.name, title: d.title, description: d.description, type: "enum", options, required: true, default: d.default }
      : { name: d.name, title: d.title, description: d.description, type: "string", required: true, pattern: d.pattern ?? "[A-Za-z0-9][A-Za-z0-9_.-]{0,62}", default: d.default };
  });
}

// ------------------------------------------------------------ dotted paths

export const splitPath = (p: string) => p.split(".").map((s) => (/^\d+$/.test(s) ? Number(s) : s));

export function getPath(obj: unknown, path: string): unknown {
  let cur: any = obj;
  for (const k of splitPath(path)) {
    if (cur === null || typeof cur !== "object" || !Object.prototype.hasOwnProperty.call(cur, k)) return undefined;
    cur = cur[k];
  }
  return cur;
}

export function setPath(obj: Record<string, unknown>, path: string, value: unknown) {
  const keys = splitPath(path);
  let cur: any = obj;
  keys.slice(0, -1).forEach((k, i) => {
    if (k === "__proto__" || k === "constructor" || k === "prototype") throw new Error(`Invalid path ${path}`);
    if (cur[k] === null || typeof cur[k] !== "object") cur[k] = typeof keys[i + 1] === "number" ? [] : {};
    cur = cur[k];
  });
  const lastKey = keys[keys.length - 1];
  if (lastKey === "__proto__" || lastKey === "constructor" || lastKey === "prototype") throw new Error(`Invalid path ${path}`);
  if (value === undefined) {
    if (Array.isArray(cur)) cur.splice(Number(lastKey), 1);
    else delete cur[lastKey];
  } else cur[lastKey] = value;
}

// ---------------------------------------------------------------- schemas

export type JsonSchema = Record<string, any>;

/** Resolves a local `$ref` (`#/$defs/x`, `#/definitions/x`) against the root schema. */
export function deref(s: JsonSchema | undefined, root: JsonSchema): JsonSchema {
  let cur = s ?? {};
  for (let i = 0; i < 10 && typeof cur.$ref === "string" && cur.$ref.startsWith("#/"); i++) {
    const target = cur.$ref
      .slice(2)
      .split("/")
      .reduce<any>((o, k) => o?.[k.replace(/~1/g, "/").replace(/~0/g, "~")], root);
    cur = { ...target, ...Object.fromEntries(Object.entries(cur).filter(([k]) => k !== "$ref")) };
  }
  return cur;
}

/** Default document for a new resource, from `default` keywords. */
export function schemaDefaults(s: JsonSchema, root: JsonSchema = s): unknown {
  const r = deref(s, root);
  if (r.default !== undefined) return structuredClone(r.default);
  if (r.type === "object" && r.properties) {
    const out: Record<string, unknown> = {};
    for (const [k, sub] of Object.entries<JsonSchema>(r.properties)) {
      const v = schemaDefaults(sub, root);
      if (v !== undefined && !(typeof v === "object" && v && !Array.isArray(v) && !Object.keys(v).length && !(r.required ?? []).includes(k))) out[k] = v;
    }
    return out;
  }
  return undefined;
}

const validators = new WeakMap<object, ValidateFunction>();

function compile(schema: JsonSchema): ValidateFunction {
  let v = validators.get(schema);
  if (!v) {
    const is2020 = typeof schema.$schema === "string" && schema.$schema.includes("2020-12");
    const ajv = is2020 ? new Ajv2020({ allErrors: true, strict: false }) : new Ajv({ allErrors: true, strict: false });
    addFormats(ajv as unknown as Ajv);
    v = ajv.compile(schema);
    validators.set(schema, v);
  }
  return v;
}

export function checkSchema(schema: unknown): string | undefined {
  if (!schema || typeof schema !== "object") return "schema must be an object";
  try {
    compile(schema as JsonSchema);
    return undefined;
  } catch (e) {
    return (e as Error).message;
  }
}

const errorPath = (e: ErrorObject) => {
  const base = e.instancePath.split("/").slice(1).map((s) => s.replace(/~1/g, "/").replace(/~0/g, "~"));
  if (e.keyword === "required") base.push(e.params.missingProperty);
  if (e.keyword === "additionalProperties") base.push(e.params.additionalProperty);
  return base.join(".") || "$";
};

/** JSON Schema validation with errors keyed by dotted field path. */
export function validateDocument(schema: JsonSchema, doc: unknown): Record<string, string> {
  const v = compile(schema);
  if (v(doc)) return {};
  const out: Record<string, string> = {};
  for (const e of v.errors ?? []) {
    if (["if", "anyOf", "oneOf"].includes(e.keyword)) continue;
    const p = errorPath(e);
    if (out[p]) continue;
    out[p] =
      e.keyword === "required" ? "is required" : e.keyword === "additionalProperties" ? "is not allowed by the schema" : e.keyword === "enum" ? `must be one of ${e.params.allowedValues.join(", ")}` : e.message ?? "is invalid";
  }
  return out;
}

/** Drops fields whose `visibleIf` is false, so hidden values never reach Git. */
export function pruneHidden(t: ResourceType, doc: Record<string, unknown>, params: Record<string, unknown>) {
  const out = structuredClone(doc);
  for (const [path, ui] of Object.entries(t.spec.ui?.fields ?? {})) {
    if (ui.visibleIf && !test(ui.visibleIf, { inputs: out, params } as Scope) && getPath(out, path) !== undefined) setPath(out, path, undefined);
  }
  return out;
}

/** Fields a user may not change after creation: declared immutable, readOnly, or Quai-managed. */
export function lockedFields(t: ResourceType, schema: JsonSchema): string[] {
  const out = new Set<string>(Object.values(t.spec.paramFields ?? {}));
  if (t.spec.ownerField) out.add(t.spec.ownerField);
  for (const [p, ui] of Object.entries(t.spec.ui?.fields ?? {})) if (ui.immutable) out.add(p);
  for (const [k, s] of Object.entries<JsonSchema>(deref(schema, schema).properties ?? {})) if (deref(s, schema).readOnly) out.add(k);
  return [...out];
}

/** Orders keys as the schema declares them (unknown keys last), so new files read like hand-written ones. */
export function orderBySchema(doc: unknown, s: JsonSchema, root: JsonSchema = s): unknown {
  const r = deref(s, root);
  if (Array.isArray(doc)) return doc.map((x) => orderBySchema(x, r.items ?? {}, root));
  if (!doc || typeof doc !== "object") return doc;
  const props = Object.keys(r.properties ?? {});
  const keys = Object.keys(doc).sort((a, b) => (props.includes(a) ? props.indexOf(a) : props.length) - (props.includes(b) ? props.indexOf(b) : props.length));
  const extra = typeof r.additionalProperties === "object" ? r.additionalProperties : {};
  return Object.fromEntries(keys.map((k) => [k, orderBySchema((doc as Record<string, unknown>)[k], r.properties?.[k] ?? extra, root)]));
}

export function serializeDocument(path: string, doc: unknown): string {
  return path.endsWith(".json") ? `${JSON.stringify(doc, null, 2)}\n` : stringify(doc, { lineWidth: 0 });
}

export function parseDocumentText(path: string, text: string): unknown {
  return path.endsWith(".json") ? JSON.parse(text) : parseYaml(text);
}

// ---------------------------------------------------------- type validation

export const TYPE_NAME = /^[a-z][a-z0-9-]{0,62}$/;
const RESERVED = [".git", ".quai", "blueprints"];

function checkExpr(src: unknown, where: string, errors: string[]) {
  if (src === undefined) return;
  try {
    parseExpr(String(src));
  } catch (e) {
    errors.push(`${where}: ${(e as Error).message}`);
  }
}

export function validateResourceType(t: any): string[] {
  const errors: string[] = [];
  if (!t || typeof t !== "object") return ["Resource type must be an object"];
  if (t.apiVersion !== "quai.dev/v1alpha1" || t.kind !== "ResourceType") errors.push("apiVersion/kind must be quai.dev/v1alpha1 ResourceType");
  if (!TYPE_NAME.test(t.metadata?.name ?? "")) errors.push("metadata.name must be a lowercase DNS label");
  if (!t.metadata?.title) errors.push("metadata.title is required");
  const spec = t.spec;
  if (!spec || typeof spec !== "object") return [...errors, "spec is required"];
  if (spec.locate?.strategy !== "file") errors.push("spec.locate.strategy must be 'file'");
  const path = spec.locate?.path;
  if (typeof path !== "string" || !path) errors.push("spec.locate.path is required");
  else {
    const p = compilePattern(path);
    if (!/\.(ya?ml|json)$/.test(path)) errors.push("spec.locate.path must end in .yaml, .yml or .json");
    if (path.startsWith("/") || path.split("/").some((s: string) => s === ".." || s === "." || s === "")) errors.push("spec.locate.path must be a clean relative path");
    if (RESERVED.includes(path.split("/")[0])) errors.push(`spec.locate.path cannot be under ${path.split("/")[0]}/`);
    if (!p.params.length) errors.push("spec.locate.path needs at least one {param}");
    if (/\}\{/.test(path)) errors.push("spec.locate.path: params must be separated by a literal");
    if (new Set(p.params).size !== p.params.length) errors.push("spec.locate.path: duplicate params");
    for (const d of spec.params ?? []) if (!p.params.includes(d?.name)) errors.push(`spec.params: '${d?.name}' is not in the path`);
    if (spec.stack !== undefined) {
      for (const m of String(spec.stack).matchAll(PARAM)) if (!p.params.includes(m[1])) errors.push(`spec.stack: '{${m[1]}}' is not a path param`);
    }
    for (const [param] of Object.entries(spec.paramFields ?? {})) if (!p.params.includes(param)) errors.push(`spec.paramFields: '${param}' is not a path param`);
  }
  if (typeof spec.schema === "string") {
    if (spec.schema.startsWith("/") || spec.schema.split("/").includes("..")) errors.push("spec.schema must be a relative path");
  } else if (!spec.schema || typeof spec.schema !== "object") errors.push("spec.schema is required (a repository path or an inline schema)");
  for (const [p, ui] of Object.entries<any>(spec.ui?.fields ?? {})) checkExpr(ui?.visibleIf, `spec.ui.fields.${p}.visibleIf`, errors);
  for (const [i, r] of (spec.approval?.rules ?? []).entries()) {
    if (!r?.name) errors.push(`spec.approval.rules[${i}]: name is required`);
    if (!Array.isArray(r?.approvers) || !r.approvers.length) errors.push(`spec.approval.rules[${i}]: approvers are required`);
    checkExpr(r?.when, `spec.approval.rules[${i}].when`, errors);
  }
  for (const [i, a] of (spec.actions ?? []).entries()) {
    if (!/^[a-z][a-z0-9-]*$/.test(a?.name ?? "") || ["update", "delete"].includes(a?.name)) errors.push(`spec.actions[${i}]: invalid or reserved name`);
    checkExpr(a?.availableIf, `spec.actions[${i}].availableIf`, errors);
    for (const k of Object.keys(a?.set ?? {})) if (!/^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$/.test(k)) errors.push(`spec.actions[${i}].set: invalid path '${k}'`);
  }
  return errors;
}

export function parseResourceType(text: string): ResourceType {
  const t = parseYaml(text);
  const errors = validateResourceType(t);
  if (errors.length) throw new Error(errors.join("\n"));
  return t as ResourceType;
}
