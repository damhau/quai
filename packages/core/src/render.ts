import { stringify } from "yaml";
import { test } from "./expr.ts";
import { renderString, renderValue, OMIT } from "./template.ts";
import { validateInputs } from "./inputs.ts";
import { getComponent, FREEFORM_PROPS, type Manifest } from "./components.ts";
import type { Blueprint, DeploymentRecord, RenderedFile, FileChange } from "./types.ts";

export class RenderError extends Error {
  constructor(message: string, public readonly field?: string) {
    super(message);
  }
}

export interface DeploymentInput {
  name: string;
  owner: string;
  inputs: Record<string, unknown>;
  createdAt: string;
  updatedAt: string;
}

export interface RenderResult {
  dir: string;
  files: RenderedFile[];
}

const SAFE_DIR = /^[A-Za-z0-9._-]+(\/[A-Za-z0-9._-]+)*$/;

export function deploymentDir(bp: Blueprint, scope: Record<string, unknown>): string {
  const raw = renderString(bp.spec.path ?? "deployments/${{ deployment.name }}", scope);
  const dir = String(raw === OMIT ? "" : raw).replace(/\/+$/, "");
  if (!SAFE_DIR.test(dir) || dir.split("/").some((s) => s === "." || s === "..") || dir.startsWith("blueprints")) {
    throw new RenderError(`Blueprint path renders to an unsafe directory: '${dir}'`);
  }
  return dir;
}

const yamlDocs = (docs: Manifest[]) => docs.map((d) => stringify(d, { lineWidth: 0 })).join("---\n");

/**
 * Renders a deployment into the files Quai commits. Rendering is pure and
 * deterministic: the same blueprint + inputs always yields the same bytes,
 * which is what makes the Git diff a trustworthy thing to approve.
 */
export function renderDeployment(bp: Blueprint, d: DeploymentInput): RenderResult {
  const deployment = { name: d.name, owner: d.owner };
  const scope = { inputs: d.inputs, deployment };
  const dir = deploymentDir(bp, scope);
  const namespace = typeof d.inputs.namespace === "string" && d.inputs.namespace ? d.inputs.namespace : d.name;
  const labels = {
    "app.kubernetes.io/managed-by": "quai",
    "quai.dev/deployment": d.name,
    "quai.dev/blueprint": bp.metadata.name,
  };

  const active = bp.spec.resources.filter((r) => test(r.when, scope));
  const activeIds = new Set(active.map((r) => r.id));
  const files: RenderedFile[] = [];

  for (const node of active) {
    const component = getComponent(node.type);
    if (!component) throw new RenderError(`Resource '${node.id}' uses unknown component '${node.type}'`);
    const rendered = renderValue(node.properties ?? {}, scope) as Record<string, unknown>;
    const { values, errors } = validateInputs(component.properties, rendered, { scopeKey: "props" });
    const firstError = Object.values(errors)[0];
    if (firstError) throw new RenderError(`Resource '${node.id}': ${firstError}`);
    for (const k of FREEFORM_PROPS[node.type] ?? []) if (k in rendered) values[k] = rendered[k];

    const targets = (bp.spec.connections ?? [])
      .filter((c) => c.from === node.id && activeIds.has(c.to))
      .map((c) => {
        const target = active.find((r) => r.id === c.to)!;
        return { node: target, component: getComponent(target.type)! };
      });

    const manifests = component.render({ node, props: values, deployment, namespace, labels, targets });
    files.push({ path: `${dir}/${node.id}.yaml`, content: yamlDocs(manifests) });
  }

  files.push({
    path: `${dir}/kustomization.yaml`,
    content: stringify({
      apiVersion: "kustomize.config.k8s.io/v1beta1",
      kind: "Kustomization",
      resources: active.map((r) => `${r.id}.yaml`),
    }),
  });

  const record: DeploymentRecord = {
    apiVersion: "quai.dev/v1alpha1",
    kind: "Deployment",
    metadata: { name: d.name, owner: d.owner, createdAt: d.createdAt, updatedAt: d.updatedAt },
    spec: { blueprint: bp.metadata.name, blueprintVersion: bp.metadata.version ?? "0.0.0", inputs: d.inputs },
  };
  files.push({ path: `${dir}/quai.yaml`, content: stringify(record) });
  return { dir, files: files.sort((a, b) => a.path.localeCompare(b.path)) };
}

/** Changes needed to turn `current` (files in a directory today) into `next`. */
export function diffFiles(current: RenderedFile[], next: RenderedFile[]): FileChange[] {
  const before = new Map(current.map((f) => [f.path, f.content]));
  const changes: FileChange[] = [];
  for (const f of next) {
    const old = before.get(f.path);
    if (old === undefined) changes.push({ path: f.path, op: "add", after: f.content });
    else if (old !== f.content) changes.push({ path: f.path, op: "modify", before: old, after: f.content });
    before.delete(f.path);
  }
  for (const [path, content] of before) changes.push({ path, op: "delete", before: content });
  return changes.sort((a, b) => a.path.localeCompare(b.path));
}

export type DiffLine = { op: " " | "+" | "-"; text: string };

/** Line-level diff (LCS) for showing reviewers exactly what will change. */
export function lineDiff(a: string, b: string): DiffLine[] {
  const x = a ? a.replace(/\n$/, "").split("\n") : [];
  const y = b ? b.replace(/\n$/, "").split("\n") : [];
  const n = x.length;
  const m = y.length;
  const lcs: number[][] = Array.from({ length: n + 1 }, () => new Array(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--)
    for (let j = m - 1; j >= 0; j--) lcs[i][j] = x[i] === y[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
  const out: DiffLine[] = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (x[i] === y[j]) {
      out.push({ op: " ", text: x[i] });
      i++;
      j++;
    } else if (lcs[i + 1][j] >= lcs[i][j + 1]) out.push({ op: "-", text: x[i++] });
    else out.push({ op: "+", text: y[j++] });
  }
  while (i < n) out.push({ op: "-", text: x[i++] });
  while (j < m) out.push({ op: "+", text: y[j++] });
  return out;
}
