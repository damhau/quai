import {
  canReview,
  compilePattern,
  checkSchema,
  deepEqual,
  diffFiles,
  formatPattern,
  getPath,
  lockedFields,
  matchPattern,
  orderBySchema,
  paramDefs,
  paramInputs,
  parseDocumentText,
  parseResourceType,
  patchYaml,
  pruneHidden,
  renderValue,
  resourceKey,
  serializeDocument,
  setPath,
  test,
  validateDocument,
  validateInputs,
  type ActionDef,
  type JsonSchema,
  type PathPattern,
  type Principal,
  type ResourceType,
} from "@quai/core";
import { BUILTIN_ACTIONS, HttpError, type Plan, type Quai } from "./service.ts";
import type { RequestKind } from "./store.ts";

// Repo-defined resource types: Quai learns the layout of an existing IaC
// repository from `.quai/types/*.yaml` and treats every matching file as a
// resource, whether Quai created it or not.

export const TYPES_DIR = ".quai/types";

export interface TypeEntry {
  type: ResourceType;
  schema: JsonSchema;
  pattern: PathPattern;
  /** Where the definition lives in the repo. */
  file: string;
}

export interface ResourceItem {
  type: string;
  key: string;
  /** `<type>/<key>`: how requests, events and status refer to it. */
  id: string;
  params: Record<string, string>;
  path: string;
  document?: Record<string, unknown>;
  owner?: string;
  /** Set when the file exists but cannot be parsed; the resource is shown but read-only. */
  error?: string;
}

const isObj = (v: unknown): v is Record<string, unknown> => !!v && typeof v === "object" && !Array.isArray(v);
const fieldErrors = (fields: Record<string, string>) => new HttpError(422, "Some fields need attention", { fields });

export class Resources {
  private cache?: { head: string; types: TypeEntry[]; errors: { path: string; error: string }[]; items?: ResourceItem[] };

  constructor(private readonly quai: Quai) {}

  private get git() {
    return this.quai.git;
  }

  private async state() {
    const head = await this.git.head();
    if (this.cache?.head === head) return this.cache;
    const types: TypeEntry[] = [];
    const errors: { path: string; error: string }[] = [];
    const { files } = await this.git.walk(TYPES_DIR);
    for (const file of files.filter((f) => /\.ya?ml$/.test(f))) {
      try {
        const type = parseResourceType((await this.git.readFile(file)) ?? "");
        if (types.some((t) => t.type.metadata.name === type.metadata.name)) throw new Error(`Duplicate type name '${type.metadata.name}'`);
        types.push({ type, schema: await this.loadSchema(type), pattern: compilePattern(type.spec.locate.path), file });
      } catch (e) {
        errors.push({ path: file, error: (e as Error).message });
      }
    }
    this.cache = { head, types, errors };
    return this.cache;
  }

  private async loadSchema(t: ResourceType): Promise<JsonSchema> {
    let schema = t.spec.schema;
    if (typeof schema === "string") {
      const text = await this.git.readFile(schema);
      if (text === undefined) throw new Error(`Schema ${schema} not found in the repository`);
      schema = parseDocumentText(schema, text) as JsonSchema;
    }
    const problem = checkSchema(schema);
    if (problem) throw new Error(`Invalid JSON Schema: ${problem}`);
    return schema;
  }

  async types() {
    const { types, errors } = await this.state();
    return { types, errors };
  }

  async type(name: string): Promise<TypeEntry> {
    const t = (await this.state()).types.find((x) => x.type.metadata.name === name);
    if (!t) throw new HttpError(404, `Resource type '${name}' not found`);
    return t;
  }

  // ---------------------------------------------------------------- inventory

  /** Every file in the repository that matches a type's path pattern. */
  async inventory(): Promise<ResourceItem[]> {
    const st = await this.state();
    if (st.items) return st.items;
    const items: ResourceItem[] = [];
    for (const t of st.types) {
      const name = t.type.metadata.name;
      for (const path of (await this.git.walk(t.pattern.prefix.replace(/\/$/, ""))).files) {
        const params = matchPattern(t.pattern, path);
        if (!params) continue;
        const key = resourceKey(t.pattern, params);
        const item: ResourceItem = { type: name, key, id: `${name}/${key}`, params, path };
        try {
          const doc = parseDocumentText(path, (await this.git.readFile(path)) ?? "");
          if (!isObj(doc)) throw new Error("File content is not a mapping");
          item.document = doc;
          const owner = t.type.spec.ownerField ? getPath(doc, t.type.spec.ownerField) : undefined;
          if (typeof owner === "string") item.owner = owner;
        } catch (e) {
          item.error = (e as Error).message;
        }
        items.push(item);
      }
    }
    st.items = items;
    return items;
  }

  async resource(type: string, key: string): Promise<{ t: TypeEntry; item: ResourceItem }> {
    const t = await this.type(type);
    const item = (await this.inventory()).find((i) => i.type === type && i.key === key);
    if (!item) throw new HttpError(404, `${type} '${key}' not found`);
    return { t, item };
  }

  /** Values offered for `discover: true` params: what already exists in the repository. */
  async discover(t: TypeEntry): Promise<Record<string, string[]>> {
    const out: Record<string, string[]> = {};
    const defs = paramDefs(t.type).filter((d) => d.discover);
    if (!defs.length) return out;
    const segments = t.pattern.source.split("/");
    const { dirs } = await this.git.walk(t.pattern.prefix.replace(/\/$/, ""));
    for (const d of defs) {
      const values = new Set<string>();
      for (const i of await this.inventory()) if (i.type === t.type.metadata.name) values.add(i.params[d.name]);
      const idx = segments.findIndex((s) => s === `{${d.name}}`);
      if (idx >= 0 && idx < segments.length - 1) {
        // A directory level: offer every directory at that position, even empty ones.
        const dirPattern = compilePattern(segments.slice(0, idx + 1).join("/"));
        for (const dir of dirs) {
          const m = matchPattern(dirPattern, dir);
          if (m) values.add(m[d.name]);
        }
      }
      out[d.name] = [...values].filter(Boolean).sort();
    }
    return out;
  }

  // --------------------------------------------------------------- permission

  canOperate(user: Principal, t: TypeEntry, item: ResourceItem) {
    if (this.quai.isAdmin(user)) return true;
    if (item.owner && item.owner === user.name) return true;
    return !!t.type.spec.operators?.length && canReview({ name: "operators", approvers: t.type.spec.operators }, user);
  }

  actions(t: TypeEntry, item: ResourceItem): ActionDef[] {
    const scope = { inputs: item.document ?? {}, params: item.params };
    return [...BUILTIN_ACTIONS, ...(t.type.spec.actions ?? []).filter((a) => test(a.availableIf, scope))];
  }

  // ----------------------------------------------------------------- planning

  private stack(t: TypeEntry, params: Record<string, string>) {
    return t.type.spec.stack ? formatPattern(t.type.spec.stack, params) : undefined;
  }

  private openRequests(id: string) {
    return this.quai.store.listRequests({ status: ["pending", "approved"], deployment: id });
  }

  /** Quai-managed fields: path params mirrored into the document, and the owner. */
  private applyManaged(t: TypeEntry, doc: Record<string, unknown>, params: Record<string, string>, owner?: string) {
    for (const [param, field] of Object.entries(t.type.spec.paramFields ?? {})) setPath(doc, field, params[param]);
    if (t.type.spec.ownerField && owner) setPath(doc, t.type.spec.ownerField, owner);
  }

  async planCreate(user: Principal, typeName: string, body: { params?: Record<string, unknown>; document?: Record<string, unknown> }): Promise<Plan> {
    const t = await this.type(typeName);
    const name = t.type.metadata.name;
    const errors: Record<string, string> = {};
    const pv = validateInputs(paramInputs(t.type, await this.discover(t)), body.params ?? {}, { scopeKey: "params" });
    for (const [k, m] of Object.entries(pv.errors)) errors[`@${k}`] = m;
    const params = pv.values as Record<string, string>;
    const lastParam = `@${t.pattern.params[t.pattern.params.length - 1]}`;

    let path = "";
    if (!Object.keys(pv.errors).length) {
      try {
        path = formatPattern(t.pattern, params);
      } catch (e) {
        errors[lastParam] = (e as Error).message;
      }
    }
    const key = resourceKey(t.pattern, params);
    const id = `${name}/${key}`;
    if (path && (await this.git.readFile(path)) !== undefined) errors[lastParam] = `${path} already exists`;
    else if (path && this.openRequests(id).length) errors[lastParam] = `A request for ${id} is already open`;

    if (!isObj(body.document ?? {})) throw new HttpError(400, "document must be an object");
    let doc = structuredClone(body.document ?? {});
    this.applyManaged(t, doc, params, user.name);
    doc = pruneHidden(t.type, doc, params);
    Object.assign(errors, validateDocument(t.schema, doc));
    if (Object.keys(errors).length) throw fieldErrors(errors);

    const files = [{ path, content: serializeDocument(path, orderBySchema(doc, t.schema)) }];
    const deployment = { name: key, owner: user.name };
    return {
      kind: "create",
      source: { name, version: t.type.metadata.version ?? "0.0.0" },
      resourceType: name,
      stack: this.stack(t, params),
      deployment: id,
      dir: path,
      inputs: doc,
      files,
      changes: diffFiles([], files),
      base: await this.git.fingerprint(path),
      rules: this.quai.rulesFor(t.type.spec.approval, { inputs: doc, params, deployment, request: { kind: "create" }, action: {}, user }),
    };
  }

  /** Day-2 on a typed resource: `update` (form edit), `delete`, or a type-defined action. */
  async planAction(
    user: Principal,
    typeName: string,
    key: string,
    actionName: string,
    body: { document?: Record<string, unknown>; inputs?: Record<string, unknown> },
  ): Promise<Plan> {
    const { t, item } = await this.resource(typeName, key);
    if (item.error || !item.document) throw new HttpError(409, `${item.path} cannot be parsed, so Quai will not edit it: ${item.error}`);
    if (!this.canOperate(user, t, item))
      throw new HttpError(403, item.owner ? `Only the owner (${item.owner}) or a platform admin can change ${item.id}` : `${item.id} has no owner on record; only platform admins can change it`);
    if (this.openRequests(item.id).length) throw new HttpError(409, `${item.id} already has an open request; wait for it or cancel it first`);

    const original = (await this.git.readFile(item.path)) ?? "";
    const current = item.document;
    const params = item.params;
    const deployment = { name: key, owner: item.owner ?? "" };
    const base = await this.git.fingerprint(item.path);
    const name = t.type.metadata.name;
    const plan = (kind: RequestKind, inputs: Record<string, unknown>, content: string | undefined, policy = t.type.spec.approval, actionInputs?: Record<string, unknown>): Plan => {
      const files = content === undefined ? [] : [{ path: item.path, content }];
      return {
        kind,
        action: kind === "action" ? actionName : undefined,
        source: { name, version: t.type.metadata.version ?? "0.0.0" },
        resourceType: name,
        stack: this.stack(t, params),
        deployment: item.id,
        dir: item.path,
        inputs,
        actionInputs,
        files,
        changes: diffFiles([{ path: item.path, content: original }], files),
        base,
        rules: this.quai.rulesFor(policy, { inputs, previous: current, params, deployment, request: { kind, action: actionName }, action: actionInputs ?? {}, user }),
      };
    };

    if (actionName === "delete") return plan("delete", current, undefined);

    let next: Record<string, unknown>;
    let policy = t.type.spec.approval;
    let actionInputs: Record<string, unknown> | undefined;
    if (actionName === "update") {
      if (!isObj(body.document)) throw new HttpError(400, "document is required");
      // Keep fields the schema does not describe (hand-added keys survive form edits).
      const known = Object.keys((t.schema.properties as object) ?? {});
      const unknown = known.length ? Object.fromEntries(Object.entries(current).filter(([k]) => !known.includes(k))) : {};
      next = { ...unknown, ...structuredClone(body.document) };
      const errors: Record<string, string> = {};
      for (const f of lockedFields(t.type, t.schema)) {
        const was = getPath(current, f);
        const now = getPath(next, f);
        if (now === undefined) setPath(next, f, was);
        else if (!deepEqual(now, was)) errors[f] = "cannot be changed after creation";
      }
      if (Object.keys(errors).length) throw fieldErrors(errors);
    } else {
      const action = t.type.spec.actions?.find((a) => a.name === actionName);
      if (!action) throw new HttpError(404, `Action '${actionName}' is not defined by type ${name}`);
      if (!test(action.availableIf, { inputs: current, params })) throw new HttpError(409, `Action '${actionName}' is not available for ${item.id}`);
      const av = validateInputs(action.inputs ?? [], body.inputs ?? {}, { scopeKey: "action", extra: { inputs: current, params } });
      if (Object.keys(av.errors).length) throw fieldErrors(av.errors);
      actionInputs = av.values;
      next = structuredClone(current);
      const set = renderValue(action.set ?? {}, { inputs: current, action: av.values, params, deployment }) as Record<string, unknown>;
      for (const [path, value] of Object.entries(set)) setPath(next, path, value);
      policy = action.approval ?? t.type.spec.approval;
    }
    this.applyManaged(t, next, params);
    next = pruneHidden(t.type, next, params);
    const errors = validateDocument(t.schema, next);
    if (Object.keys(errors).length) throw fieldErrors(errors);

    const content = item.path.endsWith(".json") ? serializeDocument(item.path, next) : patchYaml(original, next);
    if (content === original || deepEqual(parseDocumentText(item.path, content), current)) throw new HttpError(422, "This change would not modify anything");
    return plan(actionName === "update" ? "update" : "action", next, content, policy, actionInputs);
  }

  async detail(user: Principal, typeName: string, key: string) {
    const { t, item } = await this.resource(typeName, key);
    return {
      ...item,
      type: typeView(t),
      stack: this.stack(t, item.params),
      raw: (await this.git.readFile(item.path)) ?? "",
      canOperate: this.canOperate(user, t, item),
      actions: this.actions(t, item),
      sync: this.quai.store.getSync(item.id),
      history: await this.git.log({ path: item.path, limit: 30 }),
      requests: this.quai.store.listRequests({ deployment: item.id, limit: 30 }),
      events: this.quai.store.listEvents({ deployment: item.id, limit: 50 }),
    };
  }
}

/** What the UI needs to render a type's forms. */
export function typeView(t: TypeEntry, discovered: Record<string, string[]> = {}) {
  return {
    ...t.type,
    file: t.file,
    resolvedSchema: t.schema,
    paramInputs: paramInputs(t.type, discovered),
    locked: lockedFields(t.type, t.schema),
  };
}

