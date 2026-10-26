import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { parse as parseYaml } from "yaml";
import {
  compilePattern,
  formatPattern,
  lineDiff,
  lockedFields,
  matchPattern,
  orderBySchema,
  parseResourceType,
  patchYaml,
  pruneHidden,
  resourceKey,
  schemaDefaults,
  setPath,
  validateDocument,
  validateResourceType,
} from "../src/index.ts";

const repo = new URL("../../../examples/gitops-repo/", import.meta.url);
const read = (p: string) => readFileSync(new URL(p, repo), "utf8");
const vmType = parseResourceType(read(".quai/types/vm.yaml"));
const vmSchema = JSON.parse(read("catalog/schemas/vm.schema.json"));

test("path patterns match, format and refuse traversal", () => {
  const p = compilePattern("envs/{env}/vms/{name}.yaml");
  assert.equal(p.prefix, "envs/");
  assert.deepEqual(matchPattern(p, "envs/prod/vms/web01.yaml"), { env: "prod", name: "web01" });
  assert.equal(matchPattern(p, "envs/prod/vms/sub/web01.yaml"), undefined);
  assert.equal(matchPattern(p, "envs/prod/vms/.gitkeep"), undefined);
  assert.equal(resourceKey(p, { env: "prod", name: "web01" }), "prod/web01");
  assert.equal(formatPattern(p, { env: "dev", name: "x1" }), "envs/dev/vms/x1.yaml");
  assert.throws(() => formatPattern(p, { env: "..", name: "x" }));
  assert.throws(() => formatPattern(p, { env: "dev/../../etc", name: "x" }));
  assert.throws(() => formatPattern(p, { env: "dev", name: "" }));
});

test("resource type validation", () => {
  assert.deepEqual(validateResourceType(vmType), []);
  const errs = validateResourceType({
    apiVersion: "quai.dev/v1alpha1",
    kind: "ResourceType",
    metadata: { name: "x", title: "X" },
    spec: { locate: { strategy: "file", path: "../{a}{b}.txt" }, schema: {}, stack: "envs/{nope}", approval: { rules: [{ name: "r", approvers: [], when: "((" }] } },
  });
  for (const fragment of ["end in .yaml", "clean relative path", "separated", "'{nope}'", "approvers are required", "when"]) {
    assert.ok(errs.some((e) => e.includes(fragment)), `expected error containing ${fragment}: ${errs.join(" | ")}`);
  }
});

test("JSON Schema: defaults, refs, formats, errors keyed by field path", () => {
  const defaults = schemaDefaults(vmSchema) as any;
  assert.equal(defaults.cpu, 2);
  assert.deepEqual(defaults.disks, [{ size_gb: 50, tier: "standard" }]);
  assert.equal(defaults.network.vlan, 110);
  assert.deepEqual(validateDocument(vmSchema, defaults), {});

  const errors = validateDocument(vmSchema, { ...defaults, cpu: 99, extra: 1, disks: [{ tier: "ssd" }], network: { vlan: 999, ip: "not-an-ip" } });
  assert.deepEqual(Object.keys(errors).sort(), ["cpu", "disks.0.size_gb", "extra", "network.ip", "network.vlan"]);
  assert.match(errors["network.vlan"], /one of 110, 120, 130/);

  // Every hand-written example file honours the contract.
  for (const f of ["envs/prod/vms/web01.yaml", "envs/prod/vms/db01.yaml", "envs/prod/vms/legacy-ftp.yaml", "envs/dev/vms/build-agent-01.yaml"]) {
    assert.deepEqual(validateDocument(vmSchema, parseYaml(read(f))), {}, f);
  }
});

test("hidden fields are pruned, Quai-managed fields are locked", () => {
  const doc = pruneHidden(vmType, { backup: false, backup_policy: "hourly", cpu: 2 }, { env: "dev" });
  assert.deepEqual(doc, { backup: false, cpu: 2 });
  assert.deepEqual(lockedFields(vmType, vmSchema).sort(), ["network.vlan", "owner", "template"]);
});

test("setPath handles nested objects and array indices, refuses prototype keys", () => {
  const d: any = { disks: [{ size_gb: 50 }] };
  setPath(d, "disks.0.size_gb", 80);
  setPath(d, "network.vlan", 120);
  assert.deepEqual(d, { disks: [{ size_gb: 80 }], network: { vlan: 120 } });
  setPath(d, "network.vlan", undefined);
  assert.deepEqual(d.network, {});
  assert.throws(() => setPath(d, "__proto__.polluted", 1));
});

test("patchYaml changes only the edited values, keeping comments and layout", () => {
  const original = read("envs/prod/vms/web01.yaml");
  const doc = parseYaml(original);
  doc.cpu = 8;
  doc.disks[0].size_gb = 120;
  doc.tags.cost_center = "4712";
  const out = patchYaml(original, doc);
  const changed = lineDiff(original, out).filter((l) => l.op !== " ");
  const lines = (op: string) => changed.filter((l) => l.op === op).map((l) => l.text).sort();
  assert.deepEqual(lines("-"), ["  - size_gb: 80", '  cost_center: "4711"', "cpu: 4          # bumped in INC-4411"]);
  assert.deepEqual(lines("+"), ["  - size_gb: 120", '  cost_center: "4712"', "cpu: 8          # bumped in INC-4411"]);
  assert.deepEqual(parseYaml(out), doc);
});

test("patchYaml edits inside flow collections and handles structural changes", () => {
  const original = read("envs/prod/vms/db01.yaml");
  const doc = parseYaml(original);
  doc.network.ip = "10.0.13.21";
  const flow = patchYaml(original, doc);
  assert.match(flow, /network: \{ vlan: 130, ip: 10\.0\.13\.21 \}/);
  assert.match(flow, /- size_gb: 1000 {3}# data volume/);

  delete doc.backup_policy;
  doc.backup = false;
  doc.tags = { app: "orders" };
  const structural = patchYaml(original, doc);
  assert.deepEqual(parseYaml(structural), doc);
  assert.match(structural, /# data volume/, "comments survive structural edits");
  assert.equal(patchYaml(original, parseYaml(original)), original, "no-op returns the file untouched");
});

test("new files follow the schema's key order", () => {
  const doc = { backup: true, owner: "bob", network: { ip: "10.0.0.1", vlan: 110 }, cpu: 2, zzz: 1 };
  assert.deepEqual(Object.keys(orderBySchema(doc, vmSchema) as object), ["owner", "cpu", "network", "backup", "zzz"]);
  assert.deepEqual(Object.keys((orderBySchema(doc, vmSchema) as any).network), ["vlan", "ip"]);
});
