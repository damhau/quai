import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execFileSync } from "node:child_process";
import { parse } from "yaml";
import { loadConfig } from "../src/config.ts";
import { buildApp } from "../src/app.ts";

process.env.NODE_ENV = "test";
const dir = mkdtempSync(join(tmpdir(), "quai-test-"));
const remote = join(dir, "gitops.git");
let app: Awaited<ReturnType<typeof buildApp>>["app"];

const call = async (user: string, method: string, url: string, payload?: unknown) => {
  const res = await app.inject({ method: method as any, url, payload: payload as any, headers: { "x-quai-user": user } });
  return { status: res.statusCode, body: res.body ? (res.headers["content-type"]?.includes("json") ? res.json() : res.body) : undefined };
};
const remoteGit = (...args: string[]) => execFileSync("git", ["--git-dir", remote, ...args], { encoding: "utf8" });

before(async () => {
  ({ app } = await buildApp(loadConfig({ dataDir: join(dir, "data"), git: { remote } as any, simulateSync: false, webDist: undefined })));
});
after(async () => {
  await app.close();
  rmSync(dir, { recursive: true, force: true });
});

test("seeds the catalog into Git on first start", async () => {
  const { body } = await call("bob", "GET", "/api/blueprints");
  assert.deepEqual(body.items.map((b: any) => b.metadata.name).sort(), ["linux-vm", "postgres", "terraform-network", "web-service"]);
  assert.match(remoteGit("log", "--format=%s"), /seed blueprint catalog/);
});

test("dev request without approval rules is committed immediately", async () => {
  const plan = await call("bob", "POST", "/api/plan", { blueprint: "linux-vm", name: "devbox", inputs: { environment: "dev" } });
  assert.equal(plan.status, 200);
  assert.equal(plan.body.rules.length, 0);
  assert.ok(plan.body.changes.every((c: any) => c.op === "add"));

  const res = await call("bob", "POST", "/api/requests", { blueprint: "linux-vm", name: "devbox", inputs: { environment: "dev" } });
  assert.equal(res.status, 201);
  assert.equal(res.body.status, "committed");
  const vm = parse(remoteGit("show", "main:deployments/devbox/vm.yaml"));
  assert.equal(vm.kind, "VirtualMachine");
  assert.equal(vm.spec.template.spec.domain.cpu.cores, 2);
  const msg = remoteGit("log", "-1", "--format=%an|%B");
  assert.match(msg, /^Bob \(developer\)\|quai: create devbox/);
  assert.match(msg, /Quai-Request: #1/);
});

test("prod + large request needs platform-admins and finops; requester cannot self-approve", async () => {
  const body = { blueprint: "linux-vm", name: "prodbox", inputs: { environment: "prod", size: "large" }, reason: "Batch jobs" };
  const missingReason = await call("alice", "POST", "/api/requests", { ...body, reason: "" });
  assert.equal(missingReason.status, 422);

  const res = await call("alice", "POST", "/api/requests", body);
  assert.equal(res.body.status, "pending");
  assert.deepEqual(res.body.approval.map((r: any) => r.name), ["production", "large-vm-cost"]);
  const id = res.body.id;

  assert.equal((await call("alice", "POST", `/api/requests/${id}/review`, { decision: "approve" })).status, 403);
  assert.equal((await call("bob", "POST", `/api/requests/${id}/review`, { decision: "approve" })).status, 403, "bob is not eligible");

  const inbox = await call("fiona", "GET", "/api/requests?view=inbox");
  assert.deepEqual(inbox.body.map((r: any) => r.deployment), ["prodbox"]);
  const afterFinops = await call("fiona", "POST", `/api/requests/${id}/review`, { decision: "approve", comment: "Budgeted" });
  assert.equal(afterFinops.body.status, "pending");
  assert.throws(() => remoteGit("show", "main:deployments/prodbox/vm.yaml"));

  // A second platform admin is needed since alice requested it.
  assert.equal((await call("dana", "POST", `/api/requests/${id}/review`, { decision: "approve" })).status, 403);
});

test("denial rejects and requires a comment", async () => {
  const res = await call("bob", "POST", "/api/requests", { blueprint: "terraform-network", name: "net-a", inputs: {}, reason: "VPC for team" });
  const id = res.body.id;
  assert.equal((await call("nora", "POST", `/api/requests/${id}/review`, { decision: "deny" })).status, 422);
  const denied = await call("nora", "POST", `/api/requests/${id}/review`, { decision: "deny", comment: "CIDR overlaps" });
  assert.equal(denied.body.status, "rejected");
});

test("day-2 action: resize goes through policy and rewrites the deployment", async () => {
  const detail = await call("bob", "GET", "/api/deployments/devbox");
  assert.equal(detail.status, 200);
  assert.ok(detail.body.actions.some((a: any) => a.name === "resize"));
  assert.equal((await call("fiona", "POST", "/api/deployments/devbox/actions/resize", { inputs: { size: "medium" } })).status, 403, "not the owner");

  const plan = await call("bob", "POST", "/api/deployments/devbox/actions/resize/plan", { inputs: { size: "medium" } });
  assert.deepEqual(plan.body.changes.map((c: any) => `${c.op} ${c.path}`), ["modify deployments/devbox/quai.yaml", "modify deployments/devbox/vm.yaml"]);

  const res = await call("bob", "POST", "/api/deployments/devbox/actions/resize", { inputs: { size: "medium" } });
  assert.equal(res.body.status, "committed");
  const vm = parse(remoteGit("show", "main:deployments/devbox/vm.yaml"));
  assert.equal(vm.spec.template.spec.domain.cpu.cores, 4);

  const noop = await call("bob", "POST", "/api/deployments/devbox/actions/resize", { inputs: { size: "medium" } });
  assert.equal(noop.status, 422);

  const immutable = await call("bob", "POST", "/api/deployments/devbox/actions/update", { inputs: { os: "rhel-9" } });
  assert.equal(immutable.status, 422);
  assert.ok(immutable.body.details.fields.os);
});

test("stale approvals are refused when Git changed underneath", async () => {
  // Large size on an existing VM requires finops.
  const res = await call("bob", "POST", "/api/deployments/devbox/actions/resize", { inputs: { size: "large" }, reason: "Need RAM" });
  assert.equal(res.body.status, "pending");

  // Someone edits the deployment directly in Git.
  const clone = join(dir, "human");
  execFileSync("git", ["clone", "-q", remote, clone]);
  execFileSync("sh", ["-c", `cd ${clone} && echo '# hand edit' >> deployments/devbox/vm.yaml && git -c user.name=h -c user.email=h@x commit -qam edit && git push -q origin HEAD:main`]);

  const approved = await call("fiona", "POST", `/api/requests/${res.body.id}/review`, { decision: "approve" });
  assert.equal(approved.body.status, "failed");
  assert.match(approved.body.error, /changed in Git/);
});

test("delete removes the directory and the deployment disappears from inventory", async () => {
  const res = await call("bob", "POST", "/api/deployments/devbox/actions/delete", { reason: "done" });
  assert.equal(res.body.status, "committed");
  assert.throws(() => remoteGit("show", "main:deployments/devbox/quai.yaml"));
  const list = await call("bob", "GET", "/api/deployments");
  assert.equal(list.body.some((d: any) => d.name === "devbox"), false);
});

test("sync webhook updates deployment status", async () => {
  await call("bob", "POST", "/api/requests", { blueprint: "postgres", name: "orders-db", inputs: {} });
  const hook = await app.inject({ method: "POST", url: "/api/webhooks/sync", payload: { app: "orders-db", sync: "Synced", health: "Healthy", revision: "abc1234" } });
  assert.equal(hook.statusCode, 204);
  const d = await call("bob", "GET", "/api/deployments/orders-db");
  assert.equal(d.body.sync.health, "Healthy");
});

test("only admins publish blueprints, and they are validated and committed", async () => {
  const bp = (await call("alice", "GET", "/api/blueprints/postgres")).body;
  assert.equal((await call("bob", "PUT", "/api/blueprints/postgres", bp)).status, 403);
  const bad = await call("alice", "PUT", "/api/blueprints/postgres", { ...bp, spec: { ...bp.spec, resources: [{ id: "x", type: "nope" }] } });
  assert.equal(bad.status, 422);
  const ok = await call("alice", "PUT", "/api/blueprints/postgres", { ...bp, metadata: { ...bp.metadata, version: "2.1.0" } });
  assert.equal(ok.status, 200);
  assert.match(remoteGit("log", "-1", "--format=%s"), /update blueprint postgres v2.1.0/);
});
