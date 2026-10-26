import { evaluate, type Scope } from "./expr.ts";

// Templates operate on structured values, not text: a string that is exactly
// `${{ expr }}` is replaced by the typed result (number stays a number), and a
// string containing bindings is interpolated. Because manifests are serialized
// with a YAML library afterwards, user input can never inject YAML structure.

const WHOLE = /^\s*\$\{\{([\s\S]+?)\}\}\s*$/;
const PART = /\$\{\{([\s\S]+?)\}\}/g;

export const OMIT = Symbol("omit");

export function renderString(s: string, scope: Scope): unknown {
  const whole = WHOLE.exec(s);
  if (whole) {
    const v = evaluate(whole[1].trim(), scope);
    return v === undefined ? OMIT : v;
  }
  return s.replace(PART, (_, e: string) => {
    const v = evaluate(e.trim(), scope);
    return v === undefined || v === null ? "" : typeof v === "object" ? JSON.stringify(v) : String(v);
  });
}

export function renderValue(value: unknown, scope: Scope): unknown {
  if (typeof value === "string") return renderString(value, scope);
  if (Array.isArray(value)) {
    return value.map((v) => renderValue(v, scope)).filter((v) => v !== OMIT);
  }
  if (value && typeof value === "object") {
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value)) {
      const r = renderValue(v, scope);
      if (r !== OMIT) out[k] = r;
    }
    return out;
  }
  return value;
}

export function hasBinding(value: unknown): boolean {
  return typeof value === "string" && value.includes("${{");
}
