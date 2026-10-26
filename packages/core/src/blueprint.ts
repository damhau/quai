import { parse as parseYaml, stringify } from "yaml";
import { parse as parseExpr } from "./expr.ts";
import { getComponent } from "./components.ts";
import type { Blueprint, InputDef } from "./types.ts";

export const BLUEPRINT_NAME = /^[a-z][a-z0-9-]{0,62}$/;
const INPUT_TYPES = ["string", "integer", "number", "boolean", "enum"];
const RESERVED_ACTIONS = ["update", "delete"];

function checkExpr(src: unknown, where: string, errors: string[]) {
  if (src === undefined) return;
  if (typeof src !== "string") return void errors.push(`${where}: expression must be a string`);
  try {
    parseExpr(src);
  } catch (e) {
    errors.push(`${where}: ${(e as Error).message}`);
  }
}

function checkInputs(inputs: unknown, where: string, errors: string[]) {
  if (!Array.isArray(inputs)) return void errors.push(`${where} must be a list`);
  const seen = new Set<string>();
  for (const [i, d] of (inputs as InputDef[]).entries()) {
    const at = `${where}[${i}]`;
    if (!d || typeof d.name !== "string" || !/^[A-Za-z_][A-Za-z0-9_]*$/.test(d.name)) errors.push(`${at}: invalid name`);
    else if (seen.has(d.name)) errors.push(`${at}: duplicate input '${d.name}'`);
    else seen.add(d.name);
    if (!INPUT_TYPES.includes(d?.type)) errors.push(`${at}: type must be one of ${INPUT_TYPES.join(", ")}`);
    if (d?.type === "enum" && (!Array.isArray(d.options) || d.options.length === 0)) errors.push(`${at}: enum needs options`);
    if (d?.pattern !== undefined) {
      try {
        new RegExp(d.pattern);
      } catch {
        errors.push(`${at}: invalid pattern`);
      }
    }
    checkExpr(d?.visibleIf, `${at}.visibleIf`, errors);
  }
}

/** Structural validation; returns human-readable problems (empty when valid). */
export function validateBlueprint(bp: any): string[] {
  const errors: string[] = [];
  if (!bp || typeof bp !== "object") return ["Blueprint must be an object"];
  if (bp.apiVersion !== "quai.dev/v1alpha1" || bp.kind !== "Blueprint") errors.push("apiVersion/kind must be quai.dev/v1alpha1 Blueprint");
  if (!BLUEPRINT_NAME.test(bp.metadata?.name ?? "")) errors.push("metadata.name must be a lowercase DNS label");
  if (!bp.metadata?.title) errors.push("metadata.title is required");
  const spec = bp.spec;
  if (!spec || typeof spec !== "object") return [...errors, "spec is required"];
  checkInputs(spec.inputs ?? [], "spec.inputs", errors);
  if (!Array.isArray(spec.resources)) errors.push("spec.resources must be a list");
  const ids = new Set<string>();
  for (const [i, r] of (spec.resources ?? []).entries()) {
    const at = `spec.resources[${i}]`;
    if (!/^[a-z][a-z0-9-]{0,30}$/.test(r?.id ?? "")) errors.push(`${at}: id must be a short lowercase name`);
    else if (ids.has(r.id)) errors.push(`${at}: duplicate id '${r.id}'`);
    else ids.add(r.id);
    if (!getComponent(r?.type)) errors.push(`${at}: unknown component '${r?.type}'`);
    checkExpr(r?.when, `${at}.when`, errors);
  }
  for (const [i, c] of (spec.connections ?? []).entries()) {
    if (!ids.has(c?.from) || !ids.has(c?.to)) errors.push(`spec.connections[${i}]: must connect existing resources`);
  }
  for (const [i, r] of (spec.approval?.rules ?? []).entries()) {
    if (!r?.name) errors.push(`spec.approval.rules[${i}]: name is required`);
    if (!Array.isArray(r?.approvers) || r.approvers.length === 0) errors.push(`spec.approval.rules[${i}]: approvers are required`);
    checkExpr(r?.when, `spec.approval.rules[${i}].when`, errors);
  }
  for (const [i, a] of (spec.actions ?? []).entries()) {
    const at = `spec.actions[${i}]`;
    if (!/^[a-z][a-z0-9-]*$/.test(a?.name ?? "")) errors.push(`${at}: invalid name`);
    if (RESERVED_ACTIONS.includes(a?.name)) errors.push(`${at}: '${a.name}' is a built-in action`);
    checkInputs(a?.inputs ?? [], `${at}.inputs`, errors);
    checkExpr(a?.availableIf, `${at}.availableIf`, errors);
  }
  return errors;
}

export function parseBlueprint(text: string): Blueprint {
  const bp = parseYaml(text);
  const errors = validateBlueprint(bp);
  if (errors.length) throw new Error(errors.join("\n"));
  bp.spec.inputs ??= [];
  bp.spec.connections ??= [];
  bp.spec.actions ??= [];
  return bp as Blueprint;
}

export function serializeBlueprint(bp: Blueprint): string {
  return stringify(bp, { lineWidth: 0 });
}
