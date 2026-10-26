import { cp, readdir } from "node:fs/promises";
import { existsSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import { parse as parseYaml } from "yaml";
import {
  DEPLOYMENT_NAME,
  RenderError,
  canReview,
  diffFiles,
  evaluatePolicy,
  matchingRules,
  parseBlueprint,
  renderDeployment,
  renderValue,
  serializeBlueprint,
  test,
  validateBlueprint,
  validateInputs,
  type ActionDef,
  type ApprovalPolicy,
  type Blueprint,
  type DeploymentRecord,
  type Principal,
  type RenderedFile,
} from "@quai/core";
import type { QuaiConfig } from "./config.ts";
import { GitRepo } from "./git.ts";
import { Store, type RequestKind, type RequestRecord } from "./store.ts";

export class HttpError extends Error {
  constructor(public readonly status: number, message: string, public readonly details?: unknown) {
    super(message);
  }
}

export interface Deployment {
  name: string;
  dir: string;
  record: DeploymentRecord;
  sync?: { revision?: string; sync: string; health: string; message?: string; at: string };
  openRequests: number;
}

export interface Plan {
  kind: RequestKind;
  action?: string;
  blueprint: Blueprint;
  deployment: string;
  dir: string;
  inputs: Record<string, unknown>;
  actionInputs?: Record<string, unknown>;
  files: RenderedFile[];
  changes: ReturnType<typeof diffFiles>;
  base: string;
  rules: ReturnType<typeof matchingRules>;
}

const BUILTIN_ACTIONS: ActionDef[] = [
  { name: "update", title: "Edit configuration", icon: "edit", description: "Change any input of the deployment." },
  { name: "delete", title: "Delete", icon: "trash", description: "Remove the deployment from Git; the controller prunes it." },
];

export class Quai {
  readonly git: GitRepo;
  readonly store: Store;
  private blueprintCache?: { head: string; items: Blueprint[]; errors: { path: string; error: string }[] };

  constructor(readonly cfg: QuaiConfig) {
    this.git = new GitRepo({
      remote: cfg.git.remote,
      workdir: join(cfg.dataDir, "work"),
      branch: cfg.git.branch,
      committer: cfg.git.committer,
    });
    mkdirSync(cfg.dataDir, { recursive: true });
    this.store = new Store(join(cfg.dataDir, "quai.db"));
  }

  async init() {
    const seedDir = this.cfg.git.seedBlueprints;
    await this.git.init(async (workdir) => {
      if (existsSync(join(workdir, "blueprints")) || !seedDir || !existsSync(seedDir)) return false;
      await cp(seedDir, join(workdir, "blueprints"), { recursive: true });
      return true;
    });
  }

  // ---------------------------------------------------------------- identity

  resolveUser(headers: Record<string, string | string[] | undefined>): Principal {
    const h = (k: string) => {
      const v = headers[k];
      return Array.isArray(v) ? v[0] : v;
    };
    if (this.cfg.auth.mode === "proxy") {
      const name = h("x-forwarded-user") ?? h("x-forwarded-preferred-username");
      if (!name) throw new HttpError(401, "Not authenticated");
      const known = this.cfg.auth.users.find((u) => u.name === name);
      const groups = (h("x-forwarded-groups") ?? "").split(",").map((g) => g.trim()).filter(Boolean);
      return { name, email: h("x-forwarded-email") ?? known?.email, displayName: known?.displayName ?? name, groups: groups.length ? groups : known?.groups ?? [] };
    }
    const name = h("x-quai-user") ?? this.cfg.auth.users[0]?.name;
    const user = this.cfg.auth.users.find((u) => u.name === name);
    if (!user) throw new HttpError(401, `Unknown user '${name}'`);
    return user;
  }

  isAdmin(user: Principal) {
    return user.groups.some((g) => this.cfg.adminGroups.includes(g));
  }

  private author(name: string) {
    const u = this.cfg.auth.users.find((x) => x.name === name);
    return { name: u?.displayName?.replace(/[<>]/g, "") ?? name, email: u?.email ?? `${name}@users.quai` };
  }

  // -------------------------------------------------------------- blueprints

  async blueprints() {
    const head = await this.git.head();
    if (this.blueprintCache?.head === head) return this.blueprintCache;
    const items: Blueprint[] = [];
    const errors: { path: string; error: string }[] = [];
    const dir = join(this.git.workdir, "blueprints");
    const files = existsSync(dir) ? (await readdir(dir)).filter((f) => /\.ya?ml$/.test(f)).sort() : [];
    for (const f of files) {
      const text = (await this.git.readFile(`blueprints/${f}`)) ?? "";
      try {
        items.push(parseBlueprint(text));
      } catch (e) {
        errors.push({ path: `blueprints/${f}`, error: (e as Error).message });
      }
    }
    this.blueprintCache = { head, items, errors };
    return this.blueprintCache;
  }

  async blueprint(name: string): Promise<Blueprint> {
    const bp = (await this.blueprints()).items.find((b) => b.metadata.name === name);
    if (!bp) throw new HttpError(404, `Blueprint '${name}' not found`);
    return bp;
  }

  /** Blueprints are catalog code: saving one is a direct commit by its author. */
  async saveBlueprint(user: Principal, name: string, bp: unknown) {
    if (!this.isAdmin(user)) throw new HttpError(403, "Only platform admins can publish blueprints");
    const errors = validateBlueprint(bp);
    if (errors.length) throw new HttpError(422, "Blueprint is invalid", errors);
    const blueprint = bp as Blueprint;
    if (blueprint.metadata.name !== name) throw new HttpError(422, "metadata.name must match the URL");
    const path = `blueprints/${name}.yaml`;
    const existed = (await this.git.readFile(path)) !== undefined;
    const { sha, noop } = await this.git.commit({
      removeDirs: [],
      write: [{ path, content: serializeBlueprint(blueprint) }],
      message: `quai: ${existed ? "update" : "publish"} blueprint ${name} v${blueprint.metadata.version ?? "0.0.0"}`,
      author: this.author(user.name),
    });
    this.store.addEvent({ type: "blueprint.saved", actor: user.name, message: `${existed ? "Updated" : "Published"} blueprint ${name} (${sha.slice(0, 7)})` });
    return { sha, noop };
  }

  // ------------------------------------------------------------- deployments

  async deployments(): Promise<Deployment[]> {
    const open = this.store.listRequests({ status: ["pending", "approved"] });
    const out: Deployment[] = [];
    for (const path of await this.git.find("quai.yaml", ["blueprints"])) {
      try {
        const record = parseYaml((await this.git.readFile(path))!) as DeploymentRecord;
        if (record?.kind !== "Deployment") continue;
        const name = record.metadata.name;
        out.push({
          name,
          dir: path.replace(/\/quai\.yaml$/, ""),
          record,
          sync: this.store.getSync(name),
          openRequests: open.filter((r) => r.deployment === name).length,
        });
      } catch {
        // A hand-edited, broken record is skipped rather than breaking the inventory.
      }
    }
    return out.sort((a, b) => a.name.localeCompare(b.name));
  }

  async deployment(name: string): Promise<Deployment> {
    const d = (await this.deployments()).find((x) => x.name === name);
    if (!d) throw new HttpError(404, `Deployment '${name}' not found`);
    return d;
  }

  async deploymentDetail(name: string) {
    const d = await this.deployment(name);
    let blueprint: Blueprint | undefined;
    try {
      blueprint = await this.blueprint(d.record.spec.blueprint);
    } catch {
      blueprint = undefined;
    }
    const scope = { inputs: d.record.spec.inputs, deployment: { name, owner: d.record.metadata.owner } };
    const actions = [...BUILTIN_ACTIONS, ...(blueprint?.spec.actions ?? []).filter((a) => test(a.availableIf, scope))];
    return {
      ...d,
      blueprint,
      actions,
      files: await this.git.readDir(d.dir),
      history: await this.git.log({ path: d.dir, limit: 30 }),
      requests: this.store.listRequests({ deployment: name, limit: 30 }),
      events: this.store.listEvents({ deployment: name, limit: 50 }),
    };
  }

  private canOperate(user: Principal, d: Deployment) {
    return d.record.metadata.owner === user.name || this.isAdmin(user);
  }

  // ---------------------------------------------------------------- planning

  private render(bp: Blueprint, name: string, owner: string, inputs: Record<string, unknown>, createdAt: string) {
    try {
      return renderDeployment(bp, { name, owner, inputs, createdAt, updatedAt: new Date().toISOString().slice(0, 10) });
    } catch (e) {
      if (e instanceof RenderError) throw new HttpError(422, e.message);
      throw e;
    }
  }

  private rulesFor(policy: ApprovalPolicy | undefined, scope: Record<string, unknown>) {
    try {
      return matchingRules(policy, scope);
    } catch (e) {
      throw new HttpError(422, `Approval rule failed to evaluate: ${(e as Error).message}`);
    }
  }

  /** Computes exactly what a new deployment would commit, without committing. */
  async planCreate(user: Principal, body: { blueprint: string; name: string; inputs?: Record<string, unknown> }): Promise<Plan> {
    const bp = await this.blueprint(body.blueprint);
    const name = String(body.name ?? "").trim();
    const errors: Record<string, string> = {};
    if (!DEPLOYMENT_NAME.test(name)) errors.name = "Use lowercase letters, digits and dashes (max 42 characters)";
    else if ((await this.deployments()).some((d) => d.name === name)) errors.name = `A deployment named '${name}' already exists`;
    else if (this.store.listRequests({ status: ["pending", "approved"] }).some((r) => r.deployment === name && r.kind === "create"))
      errors.name = `A request for '${name}' is already open`;
    const v = validateInputs(bp.spec.inputs, body.inputs ?? {});
    Object.assign(errors, v.errors);
    if (Object.keys(errors).length) throw new HttpError(422, "Some fields need attention", { fields: errors });

    const today = new Date().toISOString().slice(0, 10);
    const r = this.render(bp, name, user.name, v.values, today);
    if ((await this.deployments()).some((d) => d.dir === r.dir) || (await this.git.readDir(r.dir)).length)
      throw new HttpError(409, `Directory ${r.dir} already exists in the repository`);
    const scope = { inputs: v.values, deployment: { name, owner: user.name }, request: { kind: "create" }, user };
    return {
      kind: "create",
      blueprint: bp,
      deployment: name,
      dir: r.dir,
      inputs: v.values,
      files: r.files,
      changes: diffFiles([], r.files),
      base: await this.git.fingerprint(r.dir),
      rules: this.rulesFor(bp.spec.approval, scope),
    };
  }

  /** Day-2: plans `update`, `delete` or a blueprint-defined action on a deployment. */
  async planAction(user: Principal, name: string, actionName: string, body: { inputs?: Record<string, unknown> }): Promise<Plan> {
    const d = await this.deployment(name);
    if (!this.canOperate(user, d)) throw new HttpError(403, `Only the owner (${d.record.metadata.owner}) or a platform admin can change '${name}'`);
    if (this.store.listRequests({ status: ["pending", "approved"], deployment: name }).length)
      throw new HttpError(409, `'${name}' already has an open request; wait for it or cancel it first`);
    const bp = await this.blueprint(d.record.spec.blueprint);
    const current = d.record.spec.inputs;
    const owner = d.record.metadata.owner;
    const deployment = { name, owner };
    const currentFiles = await this.git.readDir(d.dir);
    const base = await this.git.fingerprint(d.dir);
    const plan = (kind: RequestKind, inputs: Record<string, unknown>, files: RenderedFile[], policy: ApprovalPolicy | undefined, actionInputs?: Record<string, unknown>): Plan => ({
      kind,
      action: kind === "action" ? actionName : undefined,
      blueprint: bp,
      deployment: name,
      dir: d.dir,
      inputs,
      actionInputs,
      files,
      changes: diffFiles(currentFiles, files),
      base,
      rules: this.rulesFor(policy, { inputs, previous: current, deployment, request: { kind, action: actionName }, action: actionInputs ?? {}, user }),
    });

    if (actionName === "delete") return plan("delete", current, [], bp.spec.approval);

    let next: Record<string, unknown>;
    let policy = bp.spec.approval;
    let actionInputs: Record<string, unknown> | undefined;
    if (actionName === "update") {
      next = { ...current, ...(body.inputs ?? {}) };
      for (const def of bp.spec.inputs)
        if (def.immutable && def.name in current && def.name in next && next[def.name] !== current[def.name])
          throw new HttpError(422, "Some fields need attention", { fields: { [def.name]: `${def.title ?? def.name} cannot be changed after creation` } });
    } else {
      const action = bp.spec.actions?.find((a) => a.name === actionName);
      if (!action) throw new HttpError(404, `Action '${actionName}' is not defined by blueprint ${bp.metadata.name}`);
      if (!test(action.availableIf, { inputs: current, deployment })) throw new HttpError(409, `Action '${actionName}' is not available for '${name}'`);
      const av = validateInputs(action.inputs ?? [], body.inputs ?? {}, { scopeKey: "action", extra: { inputs: current } });
      if (Object.keys(av.errors).length) throw new HttpError(422, "Some fields need attention", { fields: av.errors });
      actionInputs = av.values;
      const set = renderValue(action.set ?? {}, { inputs: current, action: av.values, deployment }) as Record<string, unknown>;
      next = { ...current, ...set };
      policy = action.approval ?? bp.spec.approval;
    }
    const v = validateInputs(bp.spec.inputs, next);
    if (Object.keys(v.errors).length) throw new HttpError(422, "Some fields need attention", { fields: v.errors });
    const r = this.render(bp, name, owner, v.values, d.record.metadata.createdAt);
    // Keep the record's updatedAt stable when nothing else changed, so no-op requests are visible as such.
    const changed = diffFiles(currentFiles.filter((f) => !f.path.endsWith("/quai.yaml")), r.files.filter((f) => !f.path.endsWith("/quai.yaml")));
    const recordUnchanged = JSON.stringify(v.values) === JSON.stringify(current);
    if (!changed.length && recordUnchanged) throw new HttpError(422, "This change would not modify anything");
    return plan(actionName === "update" ? "update" : "action", v.values, r.files, policy, actionInputs);
  }

  // ---------------------------------------------------------------- requests

  async submit(user: Principal, plan: Plan, reason: string): Promise<RequestRecord> {
    if (plan.rules.length && !reason?.trim()) throw new HttpError(422, "Some fields need attention", { fields: { reason: "Explain why you need this; approvers will see it" } });
    const req = this.store.createRequest(
      {
        kind: plan.kind,
        action: plan.action,
        blueprint: plan.blueprint.metadata.name,
        blueprintVersion: plan.blueprint.metadata.version ?? "0.0.0",
        deployment: plan.deployment,
        dir: plan.dir,
        requester: user.name,
        reason: reason?.trim() ?? "",
        inputs: plan.inputs,
        actionInputs: plan.actionInputs,
        files: plan.files,
        changes: plan.changes,
        base: plan.base,
        rules: plan.rules,
        status: "pending",
      },
      this.cfg.requestTtlHours,
    );
    const what = plan.kind === "action" ? `${plan.action} on` : plan.kind;
    this.store.addEvent({ requestId: req.id, deployment: req.deployment, type: "request.submitted", actor: user.name, message: `Requested ${what} ${req.deployment}` });
    if (!plan.rules.length) {
      this.store.addEvent({ requestId: req.id, deployment: req.deployment, type: "request.auto-approved", actor: "quai", message: "No approval rule matched; approved automatically" });
      return this.commit(this.store.updateRequest(req.id, { status: "approved" }));
    }
    return req;
  }

  get(id: string): RequestRecord {
    this.expire();
    const r = this.store.getRequest(id);
    if (!r) throw new HttpError(404, "Request not found");
    return r;
  }

  list(user: Principal, view: "mine" | "inbox" | "all") {
    this.expire();
    if (view === "mine") return this.store.listRequests({ requester: user.name });
    if (view === "inbox") return this.store.listRequests({ status: ["pending"] }).filter((r) => this.canReviewRequest(user, r));
    return this.store.listRequests();
  }

  canReviewRequest(user: Principal, r: RequestRecord) {
    if (r.requester === user.name || r.status !== "pending") return false;
    if (r.reviews.some((x) => x.reviewer === user.name)) return false;
    const outcome = evaluatePolicy(r.rules, r.requester, r.reviews);
    return outcome.rules.some((s) => !s.satisfied && canReview(s.rule, user));
  }

  outcome(r: RequestRecord) {
    return evaluatePolicy(r.rules, r.requester, r.reviews);
  }

  async review(user: Principal, id: string, decision: "approve" | "deny", comment?: string) {
    const r = this.get(id);
    if (r.status !== "pending") throw new HttpError(409, `Request is ${r.status}`);
    if (r.requester === user.name) throw new HttpError(403, "You cannot review your own request");
    if (!this.canReviewRequest(user, r)) throw new HttpError(403, "You are not an eligible reviewer for this request");
    if (decision === "deny" && !comment?.trim()) throw new HttpError(422, "Some fields need attention", { fields: { comment: "Tell the requester why" } });
    const reviews = [...r.reviews, { reviewer: user.name, reviewerGroups: user.groups, decision, comment: comment?.trim(), at: new Date().toISOString() }];
    let updated = this.store.updateRequest(id, { reviews });
    this.store.addEvent({
      requestId: id,
      deployment: r.deployment,
      type: decision === "approve" ? "request.approved-by" : "request.denied-by",
      actor: user.name,
      message: `${decision === "approve" ? "Approved" : "Denied"}${comment ? `: ${comment}` : ""}`,
    });
    const outcome = evaluatePolicy(updated.rules, updated.requester, reviews);
    if (outcome.state === "denied") {
      updated = this.store.updateRequest(id, { status: "rejected" });
      this.store.addEvent({ requestId: id, deployment: r.deployment, type: "request.rejected", actor: "quai", message: "Request rejected" });
    } else if (outcome.state === "approved") {
      updated = await this.commit(this.store.updateRequest(id, { status: "approved" }));
    }
    return updated;
  }

  cancel(user: Principal, id: string) {
    const r = this.get(id);
    if (r.requester !== user.name && !this.isAdmin(user)) throw new HttpError(403, "Only the requester can cancel");
    if (r.status !== "pending") throw new HttpError(409, `Request is ${r.status}`);
    this.store.addEvent({ requestId: id, deployment: r.deployment, type: "request.cancelled", actor: user.name, message: "Cancelled" });
    return this.store.updateRequest(id, { status: "cancelled" });
  }

  /** Retry a request whose commit failed (e.g. the remote was unreachable). */
  async retry(user: Principal, id: string) {
    const r = this.get(id);
    if (r.requester !== user.name && !this.isAdmin(user)) throw new HttpError(403, "Only the requester can retry");
    if (r.status !== "failed") throw new HttpError(409, `Request is ${r.status}`);
    return this.commit(this.store.updateRequest(id, { status: "approved", error: undefined }));
  }

  private expire() {
    const now = new Date().toISOString();
    for (const r of this.store.listRequests({ status: ["pending"] })) {
      if (r.expiresAt < now) {
        this.store.updateRequest(r.id, { status: "expired" });
        this.store.addEvent({ requestId: r.id, deployment: r.deployment, type: "request.expired", actor: "quai", message: "Expired without enough approvals" });
      }
    }
  }

  /**
   * Commits exactly the files that were reviewed. If the deployment changed in
   * Git since the request was planned, the approval is stale and the commit is
   * refused rather than silently applying something nobody looked at.
   */
  private async commit(r: RequestRecord): Promise<RequestRecord> {
    const approvers = r.reviews.filter((x) => x.decision === "approve").map((x) => x.reviewer);
    const verb = r.kind === "create" ? "create" : r.kind === "delete" ? "delete" : r.kind === "update" ? "update" : r.action;
    const author = this.author(r.requester);
    const message = [
      `quai: ${verb} ${r.deployment} (${r.blueprint})`,
      "",
      r.reason || "No reason given.",
      "",
      `Quai-Request: #${r.number} ${r.id}`,
      `Requested-by: ${author.name} <${author.email}>`,
      ...approvers.map((a) => {
        const x = this.author(a);
        return `Approved-by: ${x.name} <${x.email}>`;
      }),
    ].join("\n");
    try {
      const { sha } = await this.git.commit({ removeDirs: [r.dir], write: r.files, message, author }, async () => {
        if ((await this.git.fingerprint(r.dir)) !== r.base)
          throw new HttpError(409, `${r.dir} changed in Git after this request was submitted; submit a new request`);
      });
      this.store.addEvent({ requestId: r.id, deployment: r.deployment, type: "request.committed", actor: "quai", message: `Committed ${sha.slice(0, 7)} to ${this.cfg.git.branch}` });
      this.store.setSync({ deployment: r.deployment, revision: sha, sync: "OutOfSync", health: r.kind === "delete" ? "Deleting" : "Progressing", message: "Waiting for the GitOps controller" });
      if (this.cfg.simulateSync) this.simulateReconcile(r.deployment, sha, r.kind === "delete");
      return this.store.updateRequest(r.id, { status: "committed", commit: sha });
    } catch (e) {
      const msg = (e as Error).message;
      this.store.addEvent({ requestId: r.id, deployment: r.deployment, type: "request.failed", actor: "quai", message: msg });
      return this.store.updateRequest(r.id, { status: "failed", error: msg });
    }
  }

  // -------------------------------------------------------------- sync state

  /** Receives status from Argo CD notifications, Flux alerts or any CI job. */
  reportSync(body: { deployment?: string; app?: string; revision?: string; sync?: string; health?: string; message?: string }) {
    const deployment = body.deployment ?? body.app;
    if (!deployment || !DEPLOYMENT_NAME.test(deployment)) throw new HttpError(422, "deployment is required");
    this.store.setSync({ deployment, revision: body.revision, sync: body.sync ?? "Unknown", health: body.health ?? "Unknown", message: body.message });
    this.store.addEvent({ deployment, type: "sync", actor: "gitops", message: `${body.sync ?? "Unknown"} / ${body.health ?? "Unknown"}${body.revision ? ` at ${body.revision.slice(0, 7)}` : ""}` });
  }

  private simulateReconcile(deployment: string, revision: string, deleted: boolean) {
    setTimeout(() => {
      try {
        this.reportSync({ deployment, revision, sync: "Synced", health: deleted ? "Deleted" : "Healthy", message: "Simulated reconcile (simulateSync is on)" });
      } catch {
        // The store may be closed during shutdown.
      }
    }, 2500).unref();
  }
}
