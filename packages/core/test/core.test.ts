import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import { parse as parseYaml } from "yaml";
import {
  evaluate,
  renderValue,
  validateInputs,
  evaluatePolicy,
  matchingRules,
  parseBlueprint,
  renderDeployment,
  diffFiles,
  lineDiff,
  validateBlueprint,
} from "../src/index.ts";

const examples = new URL("../../../examples/gitops-repo/blueprints/", import.meta.url);
const load = (name: string) => parseBlueprint(readFileSync(new URL(`${name}.yaml`, examples), "utf8"));

test("expressions: precedence, ternaries, membership", () => {
  const s = { inputs: { env: "prod", size: 3, tags: ["a", "b"] } };
  assert.equal(evaluate("inputs.env == 'prod' && inputs.size > 2", s), true);
  assert.equal(evaluate("1 + 2 * 3", s), 7);
  assert.equal(evaluate("inputs.size == 1 ? 'a' : inputs.size == 3 ? 'c' : 'z'", s), "c");
  assert.equal(evaluate("inputs.env in ['dev', 'prod']", s), true);
  assert.equal(evaluate("'b' in inputs.tags", s), true);
  assert.equal(evaluate("!(inputs.size < 2)", s), true);
  assert.equal(evaluate("inputs.missing.deep", s), undefined);
});

test("expressions cannot reach prototypes or run code", () => {
  assert.equal(evaluate("inputs.constructor", { inputs: {} }), undefined);
  assert.equal(evaluate("inputs.__proto__", { inputs: {} }), undefined);
  assert.throws(() => evaluate("inputs.x; process.exit()", { inputs: {} }));
  assert.throws(() => evaluate("inputs['a']", { inputs: {} }));
});

test("templates keep types and never inject structure", () => {
  const out = renderValue(
    { n: "${{ inputs.n }}", s: "x-${{ inputs.n }}", evil: "${{ inputs.evil }}", gone: "${{ inputs.nope }}" },
    { inputs: { n: 4, evil: "a\nkind: Secret" } },
  );
  assert.deepEqual(out, { n: 4, s: "x-4", evil: "a\nkind: Secret" });
});

test("input validation: defaults, coercion, visibility, errors", () => {
  const bp = load("postgres");
  const ok = validateInputs(bp.spec.inputs, { environment: "dev", storageGB: "50", highAvailability: true });
  assert.deepEqual(ok.errors, {});
  assert.equal(ok.values.storageGB, 50);
  assert.equal("highAvailability" in ok.values, false, "hidden outside prod");
  const prod = validateInputs(bp.spec.inputs, { environment: "prod" });
  assert.equal(prod.values.highAvailability, true);
  const bad = validateInputs(bp.spec.inputs, { environment: "staging", storageGB: 5, namespace: "Bad_NS" });
  assert.deepEqual(Object.keys(bad.errors).sort(), ["environment", "namespace", "storageGB"]);
});

test("approval policy: matching rules, thresholds, self-approval, denial", () => {
  const bp = load("linux-vm");
  const rules = matchingRules(bp.spec.approval, { inputs: { environment: "prod", size: "large" } });
  assert.deepEqual(rules.map((r) => r.name), ["production", "large-vm-cost"]);
  const at = new Date().toISOString();
  const admin = { reviewer: "alice", reviewerGroups: ["platform-admins"], decision: "approve" as const, at };
  const finops = { reviewer: "fiona", reviewerGroups: ["finops"], decision: "approve" as const, at };
  assert.equal(evaluatePolicy(rules, "bob", [admin]).state, "pending");
  assert.equal(evaluatePolicy(rules, "bob", [admin, finops]).state, "approved");
  assert.equal(evaluatePolicy(rules, "alice", [admin, finops]).state, "pending", "requester cannot approve own request");
  assert.equal(evaluatePolicy(rules, "bob", [{ ...finops, decision: "deny" }]).state, "denied");
  assert.equal(evaluatePolicy([], "bob", []).state, "approved");
});

test("every example blueprint is valid and renders", () => {
  for (const f of readdirSync(examples)) {
    const bp = parseBlueprint(readFileSync(new URL(f, examples), "utf8"));
    const { values, errors } = validateInputs(bp.spec.inputs, {});
    assert.deepEqual(errors, {}, f);
    const r = renderDeployment(bp, { name: "demo", owner: "bob", inputs: values, createdAt: "t", updatedAt: "t" });
    assert.ok(r.files.some((x) => x.path === `deployments/demo/quai.yaml`), f);
  }
});

test("rendering wires connections and honours `when`", () => {
  const bp = load("web-service");
  const inputs = validateInputs(bp.spec.inputs, { environment: "prod", withBucket: true }).values;
  const r = renderDeployment(bp, { name: "shop", owner: "bob", inputs, createdAt: "t", updatedAt: "t" });
  assert.deepEqual(r.files.map((f) => f.path.split("/").pop()), ["app.yaml", "assets.yaml", "db.yaml", "kustomization.yaml", "ns.yaml", "quai.yaml"]);
  const app = parseYaml(r.files.find((f) => f.path.endsWith("app.yaml"))!.content);
  const params = app.spec.source.helm.parameters.map((p: any) => p.name);
  assert.ok(params.includes("connections.db.secretName"));
  assert.ok(params.includes("connections.assets.secretName"));
  const db = parseYaml(r.files.find((f) => f.path.endsWith("db.yaml"))!.content);
  assert.equal(db.spec.parameters.highAvailability, true);
  assert.equal(db.metadata.namespace, "shop");

  const noBucket = renderDeployment(bp, { name: "shop", owner: "bob", inputs: { ...inputs, withBucket: false }, createdAt: "t", updatedAt: "t" });
  const changes = diffFiles(r.files, noBucket.files);
  assert.deepEqual(changes.map((c) => `${c.op} ${c.path.split("/").pop()}`), ["modify app.yaml", "delete assets.yaml", "modify kustomization.yaml", "modify quai.yaml"]);
});

test("unsafe deployment paths are rejected", () => {
  const bp = load("linux-vm");
  bp.spec.path = "../${{ deployment.name }}";
  assert.throws(() => renderDeployment(bp, { name: "x", owner: "o", inputs: {}, createdAt: "t", updatedAt: "t" }), /unsafe/);
  bp.spec.path = "blueprints/${{ deployment.name }}";
  assert.throws(() => renderDeployment(bp, { name: "x", owner: "o", inputs: {}, createdAt: "t", updatedAt: "t" }), /unsafe/);
});

test("blueprint validation reports useful errors", () => {
  const errs = validateBlueprint({
    apiVersion: "quai.dev/v1alpha1",
    kind: "Blueprint",
    metadata: { name: "Bad Name" },
    spec: { inputs: [{ name: "x", type: "enum" }], resources: [{ id: "a", type: "nope", when: "((" }], actions: [{ name: "delete" }] },
  });
  assert.ok(errs.some((e) => e.includes("metadata.name")));
  assert.ok(errs.some((e) => e.includes("enum needs options")));
  assert.ok(errs.some((e) => e.includes("unknown component")));
  assert.ok(errs.some((e) => e.includes("when")));
  assert.ok(errs.some((e) => e.includes("built-in action")));
});

test("line diff", () => {
  assert.deepEqual(lineDiff("a\nb\nc\n", "a\nc\nd\n"), [
    { op: " ", text: "a" },
    { op: "-", text: "b" },
    { op: " ", text: "c" },
    { op: "+", text: "d" },
  ]);
});
