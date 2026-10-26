import { isScalar, parseDocument, stringify } from "yaml";

// Edits a human-maintained YAML file so the diff shows only what changed.
// Re-serializing a parsed document is not enough: the library re-aligns
// trailing comments and reflows flow collections, so every PR would touch
// unrelated lines. Value changes are spliced into the original text at the
// node's source range; only structural changes (keys added or removed, lists
// growing) fall back to editing the document tree, which keeps comments but
// may normalize spacing.

type Path = (string | number)[];
type Change = { path: Path; value?: unknown; op: "set" | "delete"; scalar: boolean };

const isObj = (v: unknown): v is Record<string, unknown> => !!v && typeof v === "object" && !Array.isArray(v);

export function deepEqual(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  if (Array.isArray(a) && Array.isArray(b)) return a.length === b.length && a.every((x, i) => deepEqual(x, b[i]));
  if (isObj(a) && isObj(b)) {
    const ka = Object.keys(a);
    const kb = Object.keys(b);
    return ka.length === kb.length && ka.every((k) => Object.prototype.hasOwnProperty.call(b, k) && deepEqual(a[k], b[k]));
  }
  return false;
}

function changes(a: unknown, b: unknown, path: Path, out: Change[]) {
  if (deepEqual(a, b)) return;
  if (isObj(a) && isObj(b)) {
    for (const k of Object.keys(a)) if (!(k in b)) out.push({ path: [...path, k], op: "delete", scalar: false });
    for (const k of Object.keys(b)) {
      if (!(k in a)) out.push({ path: [...path, k], op: "set", value: b[k], scalar: false });
      else changes(a[k], b[k], [...path, k], out);
    }
    return;
  }
  if (Array.isArray(a) && Array.isArray(b) && a.length === b.length) {
    a.forEach((x, i) => changes(x, b[i], [...path, i], out));
    return;
  }
  const scalar = (v: unknown) => v === null || ["string", "number", "boolean"].includes(typeof v);
  out.push({ path, op: "set", value: b, scalar: scalar(a) && scalar(b) && !(typeof b === "string" && b.includes("\n")) });
}

function spliceScalars(text: string, list: Change[]): string | undefined {
  const doc = parseDocument(text, { keepSourceTokens: false });
  const edits: { start: number; end: number; text: string }[] = [];
  for (const c of list) {
    const node = doc.getIn(c.path, true);
    if (!isScalar(node) || !node.range) return undefined;
    const [start, end] = node.range;
    if (end <= start) return undefined; // implicit null (`key:`), nothing to replace
    edits.push({ start, end, text: stringify(c.value, { lineWidth: 0 }).trimEnd() });
  }
  let out = text;
  for (const e of edits.sort((x, y) => y.start - x.start)) out = out.slice(0, e.start) + e.text + out.slice(e.end);
  return out;
}

/** Returns `original` edited to represent `next`, with the smallest diff it can. */
export function patchYaml(original: string, next: unknown): string {
  const doc = parseDocument(original);
  if (doc.errors.length) throw new Error(`Cannot edit invalid YAML: ${doc.errors[0].message}`);
  const current = doc.toJS();
  const list: Change[] = [];
  changes(current, next, [], list);
  if (!list.length) return original;

  if (list.every((c) => c.scalar && c.path.length > 0)) {
    const spliced = spliceScalars(original, list);
    if (spliced !== undefined && deepEqual(parseDocument(spliced).toJS(), next)) return spliced;
  }
  if (!isObj(next) || !isObj(current)) return stringify(next, { lineWidth: 0 });
  for (const c of list) {
    if (c.op === "delete") doc.deleteIn(c.path);
    else doc.setIn(c.path, doc.createNode(c.value));
  }
  const out = doc.toString({ lineWidth: 0 });
  if (!deepEqual(parseDocument(out).toJS(), next)) return stringify(next, { lineWidth: 0 });
  return out;
}
