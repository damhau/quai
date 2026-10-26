import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execFileSync } from "node:child_process";
import { parse } from "yaml";
import { lineDiff } from "@quai/core";
import { loadConfig } from "../src/config.ts";
import { buildApp } from "../src/app.ts";

process.env.NODE_ENV = "test";
const dir = mkdtempSync(join(tmpdir(), "quai-types-"));
const remote = join(dir, "gitops.git");
let app: Awaited<ReturnType<typeof buildApp>>["app"];

const call = async (user: string, method: string, url: string, payload?: unknown) => {
  const res = await app.inject({ method: method as any, url, payload: payload as any, headers: { "x-quai-user": user } });
  return { status: res.statusCode, body: res.body ? res.json() : undefined };
};
const show = (path: string) => execFileSync("git", ["--git-dir", remote, "show", `main:${path}`], { encoding: "utf8" });
const lastCommit = () => execFileSync("git", ["--git-dir", remote, "log", "-1", "--format=%an|%B"], { encoding: "utf8" });
const humanCommit = (script: string) => {
  const clone = join(dir, `human-${Date.now()}`);
  execFileSync("git", ["clone", "-q", remote, clone]);
  execFileSync("sh", ["-c", `cd ${clone} && ${script} && git add -A && git -c user.name=h -c user.email=h@x commit -qm human && git push -q origin HEAD:main`]);
};
const key = (k: string) => encodeURIComponent(k);
const newVm = { template: "ubuntu-24.04", cpu: 2, memory_gb: 4, disks: [{ size_gb: 50, tier: "standard" }], network: { vlan: 110 }, backup: true, backup_policy: "daily" };

before(async () => {
  ({ app } = await buildApp(loadConfig({ dataDir: join(dir, "data"), git: { remote } as any, simulateSync: false, webDist: undefined })));
});
after(async () => {
  await app.close();
  rmSync(dir, { recursive: true, force: true });
});

test("types are read from the repository, with discovered params", async () => {
  const { body } = await call("bob", "GET", "/api/types");
  assert.deepEqual(body.errors, []);
  const vm = body.items.find((t: any) => t.metadata.name === "vm");
  assert.equal(vm.count, 4, "pre-existing files are part of the inventory");
  assert.deepEqual(vm.paramInputs.find((p: any) => p.name === "env").options, ["dev", "prod", "test"], "test env is discovered from its empty directory");
  assert.equal(vm.resolvedSchema.title, "Virtual machine");
});

test("brownfield files show up in the inventory with owners", async () => {
  const { body } = await call("bob", "GET", "/api/inventory");
  const vms = body.filter((i: any) => i.source === "vm");
  assert.deepEqual(vms.map((i: any) => `${i.id}:${i.owner ?? "-"}`).sort(), ["vm/dev/build-agent-01:bob", "vm/prod/db01:dana", "vm/prod/legacy-ftp:-", "vm/prod/web01:bob"]);
});

test("create writes one file at the type's path, owned by the requester", async () => {
  const plan = await call("bob", "POST", "/api/types/vm/plan", { params: { env: "dev", name: "api01" }, document: newVm });
  assert.equal(plan.status, 200, JSON.stringify(plan.body));
  assert.equal(plan.body.stack, "envs/dev");
  assert.deepEqual(plan.body.changes.map((c: any) => `${c.op} ${c.path}`), ["add envs/dev/vms/api01.yaml"]);
  assert.deepEqual(plan.body.rules, []);

  const res = await call("bob", "POST", "/api/types/vm/requests", { params: { env: "dev", name: "api01" }, document: newVm });
  assert.equal(res.body.status, "committed", JSON.stringify(res.body));
  const file = parse(show("envs/dev/vms/api01.yaml"));
  assert.equal(file.owner, "bob");
  assert.equal(file.cpu, 2);
  const msg = lastCommit();
  assert.match(msg, /^Bob \(developer\)\|quai: create vm\/dev\/api01/);
  assert.match(msg, /Quai-Stack: envs\/dev/);
});

test("create validates params and the document against the repo's JSON Schema", async () => {
  const bad = await call("bob", "POST", "/api/types/vm/plan", { params: { env: "staging", name: "../etc" }, document: { ...newVm, cpu: 64, network: { vlan: 999 }, extra: true } });
  assert.equal(bad.status, 422);
  assert.deepEqual(Object.keys(bad.body.details.fields).sort(), ["@env", "@name", "cpu", "extra", "network.vlan"]);
  const dup = await call("bob", "POST", "/api/types/vm/plan", { params: { env: "prod", name: "web01" }, document: newVm });
  assert.match(dup.body.details.fields["@name"], /already exists/);
  // Hidden fields never reach Git.
  const hidden = await call("bob", "POST", "/api/types/vm/plan", { params: { env: "dev", name: "nobackup" }, document: { ...newVm, backup: false } });
  assert.equal(hidden.body.inputs.backup_policy, undefined);
});

test("prod create needs platform-admins; growing past 8 vCPU needs finops", async () => {
  const res = await call("bob", "POST", "/api/types/vm/requests", { params: { env: "prod", name: "batch02" }, document: { ...newVm, cpu: 16 }, reason: "Batch" });
  assert.equal(res.body.status, "pending");
  assert.deepEqual(res.body.approval.map((r: any) => r.name), ["production", "large-vm"]);
  await call("alice", "POST", `/api/requests/${res.body.id}/review`, { decision: "approve" });
  const done = await call("fiona", "POST", `/api/requests/${res.body.id}/review`, { decision: "approve" });
  assert.equal(done.body.status, "committed");
  assert.match(lastCommit(), /Approved-by: Alice.*\nApproved-by: Fiona/);
});

test("day-2 resize on a hand-written file changes only the touched lines", async () => {
  const before = show("envs/prod/vms/web01.yaml");
  const plan = await call("bob", "POST", `/api/types/vm/resources/${key("prod/web01")}/actions/resize/plan`, { inputs: { cpu: 6, memory_gb: 24 } });
  assert.equal(plan.status, 200, JSON.stringify(plan.body));
  assert.deepEqual(plan.body.rules.map((r: any) => r.name), ["production"], "6 vCPU is not large");
  const res = await call("bob", "POST", `/api/types/vm/resources/${key("prod/web01")}/actions/resize`, { inputs: { cpu: 6, memory_gb: 24 }, reason: "Black Friday" });
  await call("alice", "POST", `/api/requests/${res.body.id}/review`, { decision: "approve" });
  const after = show("envs/prod/vms/web01.yaml");
  const changed = lineDiff(before, after).filter((l) => l.op !== " ").map((l) => `${l.op}${l.text}`).sort();
  assert.deepEqual(changed, ["+cpu: 6          # bumped in INC-4411", "+memory_gb: 24", "-cpu: 4          # bumped in INC-4411", "-memory_gb: 16"]);
  assert.match(after, /^# Front-end for the shop/);
});

test("indexed set paths, immutable fields, owner protection and no-ops", async () => {
  const extend = await call("bob", "POST", `/api/types/vm/resources/${key("dev/build-agent-01")}/actions/extend-disk`, { inputs: { size_gb: 200 } });
  assert.equal(extend.body.status, "committed", JSON.stringify(extend.body));
  assert.equal(parse(show("envs/dev/vms/build-agent-01.yaml")).disks[0].size_gb, 200);

  const detail = (await call("bob", "GET", `/api/types/vm/resources/${key("dev/build-agent-01")}`)).body;
  const locked = await call("bob", "POST", `/api/types/vm/resources/${key("dev/build-agent-01")}/actions/update/plan`, { document: { ...detail.document, template: "rhel-9" } });
  assert.equal(locked.status, 422);
  assert.ok(locked.body.details.fields.template);
  const owner = await call("bob", "POST", `/api/types/vm/resources/${key("dev/build-agent-01")}/actions/update/plan`, { document: { ...detail.document, owner: "mallory" } });
  assert.ok(owner.body.details.fields.owner, "owner cannot be reassigned through a form edit");

  const noop = await call("bob", "POST", `/api/types/vm/resources/${key("dev/build-agent-01")}/actions/update/plan`, { document: detail.document });
  assert.equal(noop.status, 422);
  assert.match(noop.body.error, /would not modify/);
});

test("only owners, operators or admins can change a resource", async () => {
  assert.equal((await call("bob", "POST", `/api/types/vm/resources/${key("prod/db01")}/actions/resize/plan`, { inputs: { cpu: 16, memory_gb: 64 } })).status, 403);
  const legacy = await call("bob", "POST", `/api/types/vm/resources/${key("prod/legacy-ftp")}/actions/delete/plan`, {});
  assert.equal(legacy.status, 403);
  assert.match(legacy.body.error, /no owner on record/);
  assert.equal((await call("alice", "POST", `/api/types/vm/resources/${key("prod/legacy-ftp")}/actions/delete/plan`, {})).status, 200);
});

test("a human edit to the file makes a pending approval stale", async () => {
  const res = await call("dana", "POST", `/api/types/vm/resources/${key("prod/db01")}/actions/resize`, { inputs: { cpu: 16, memory_gb: 96 }, reason: "More RAM" });
  assert.equal(res.body.status, "pending");
  humanCommit("sed -i 's/backup_policy: hourly/backup_policy: daily/' envs/prod/vms/db01.yaml");
  const approved = await call("alice", "POST", `/api/requests/${res.body.id}/review`, { decision: "approve" });
  assert.equal(approved.body.status, "failed");
  assert.match(approved.body.error, /changed in Git/);
});

test("delete removes the file; unparseable files are listed but never edited", async () => {
  const del = await call("bob", "POST", `/api/types/vm/resources/${key("dev/api01")}/actions/delete`, {});
  assert.equal(del.body.status, "committed");
  assert.throws(() => show("envs/dev/vms/api01.yaml"));

  humanCommit("printf 'cpu: [unclosed\\n' > envs/dev/vms/broken.yaml");
  await app.inject({ method: "POST", url: "/api/git/sync", headers: { "x-quai-user": "alice" } });
  const inv = (await call("alice", "GET", "/api/inventory")).body;
  const broken = inv.find((i: any) => i.id === "vm/dev/broken");
  assert.ok(broken?.error);
  const edit = await call("alice", "POST", `/api/types/vm/resources/${key("dev/broken")}/actions/delete/plan`, {});
  assert.equal(edit.status, 409);
});
