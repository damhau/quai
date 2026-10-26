import { test, type Scope } from "./expr.ts";
import type { InputDef } from "./types.ts";

export interface ValidationResult {
  values: Record<string, unknown>;
  errors: Record<string, string>;
}

export function optionValues(def: InputDef): string[] {
  return (def.options ?? []).map((o) => (typeof o === "string" ? o : o.value));
}

export function defaultsFor(defs: InputDef[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const d of defs) {
    if (d.default !== undefined) out[d.name] = d.default;
    else if (d.type === "boolean") out[d.name] = false;
    else if (d.type === "enum" && d.required) out[d.name] = optionValues(d)[0];
  }
  return out;
}

/** Is the field visible given the current values? `scopeKey` is the name the
 * values are exposed as in expressions (`inputs` or `action`). */
export function isVisible(def: InputDef, values: Record<string, unknown>, scopeKey = "inputs", extra: Scope = {}): boolean {
  return test(def.visibleIf, { ...extra, [scopeKey]: values });
}

function coerce(def: InputDef, raw: unknown): unknown {
  if (raw === undefined || raw === null || raw === "") return undefined;
  switch (def.type) {
    case "integer":
    case "number": {
      const n = typeof raw === "number" ? raw : Number(raw);
      return Number.isNaN(n) ? raw : n;
    }
    case "boolean":
      return raw === true || raw === "true";
    default:
      return typeof raw === "string" ? raw.trim() : raw;
  }
}

/**
 * Validates and normalizes request values: applies defaults, coerces types,
 * drops hidden or unknown fields. Runs identically in the browser and on the
 * server, and the server is the authority.
 */
export function validateInputs(
  defs: InputDef[],
  raw: Record<string, unknown>,
  opts: { scopeKey?: string; extra?: Scope } = {},
): ValidationResult {
  const scopeKey = opts.scopeKey ?? "inputs";
  const values: Record<string, unknown> = {};
  const errors: Record<string, string> = {};
  const merged = { ...defaultsFor(defs), ...raw };

  for (const def of defs) {
    const v = coerce(def, merged[def.name]);
    if (v !== undefined) values[def.name] = v;
  }
  for (const def of defs) {
    if (!isVisible(def, values, scopeKey, opts.extra)) {
      delete values[def.name];
      continue;
    }
    const v = values[def.name];
    const label = def.title ?? def.name;
    if (v === undefined) {
      if (def.required) errors[def.name] = `${label} is required`;
      continue;
    }
    switch (def.type) {
      case "integer":
      case "number":
        if (typeof v !== "number" || Number.isNaN(v)) errors[def.name] = `${label} must be a number`;
        else if (def.type === "integer" && !Number.isInteger(v)) errors[def.name] = `${label} must be a whole number`;
        else if (def.min !== undefined && v < def.min) errors[def.name] = `${label} must be at least ${def.min}`;
        else if (def.max !== undefined && v > def.max) errors[def.name] = `${label} must be at most ${def.max}`;
        break;
      case "boolean":
        if (typeof v !== "boolean") errors[def.name] = `${label} must be true or false`;
        break;
      case "enum":
        if (!optionValues(def).includes(String(v))) errors[def.name] = `${label} must be one of ${optionValues(def).join(", ")}`;
        break;
      case "string":
        if (typeof v !== "string") errors[def.name] = `${label} must be text`;
        else if (def.pattern && !new RegExp(`^(?:${def.pattern})$`).test(v)) errors[def.name] = `${label} has an invalid format`;
        else if (def.min !== undefined && v.length < def.min) errors[def.name] = `${label} must be at least ${def.min} characters`;
        else if (def.max !== undefined && v.length > def.max) errors[def.name] = `${label} must be at most ${def.max} characters`;
        break;
    }
  }
  return { values, errors };
}

/** DNS-1123 label: what Kubernetes accepts as a resource name. */
export const DEPLOYMENT_NAME = /^[a-z]([-a-z0-9]{0,40}[a-z0-9])?$/;
