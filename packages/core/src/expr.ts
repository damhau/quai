// A tiny, side-effect free expression language used for `when`, `visibleIf`
// and `${{ }}` template bindings. It never touches `eval`/`Function`, so
// blueprint authors cannot run arbitrary code on the server or in the browser.
//
// Supported: literals (numbers, 'strings', "strings", true, false, null),
// arrays [a, b], dotted paths (inputs.size, deployment.name), parentheses,
// ! && || == != < <= > >= in + - * / and the ternary `a ? b : c`.

export type Scope = Record<string, unknown>;

type Token =
  | { t: "num"; v: number }
  | { t: "str"; v: string }
  | { t: "id"; v: string }
  | { t: "op"; v: string };

const OPS = ["==", "!=", "<=", ">=", "&&", "||", "<", ">", "!", "(", ")", "[", "]", ",", ".", "+", "-", "*", "/", "?", ":"];

export class ExprError extends Error {}

function tokenize(src: string): Token[] {
  const out: Token[] = [];
  let i = 0;
  while (i < src.length) {
    const c = src[i];
    if (/\s/.test(c)) {
      i++;
      continue;
    }
    if (/[0-9]/.test(c)) {
      const m = /^[0-9]+(\.[0-9]+)?/.exec(src.slice(i))!;
      out.push({ t: "num", v: Number(m[0]) });
      i += m[0].length;
      continue;
    }
    if (c === "'" || c === '"') {
      let j = i + 1;
      let s = "";
      while (j < src.length && src[j] !== c) {
        if (src[j] === "\\" && j + 1 < src.length) j++;
        s += src[j++];
      }
      if (j >= src.length) throw new ExprError(`Unterminated string in: ${src}`);
      out.push({ t: "str", v: s });
      i = j + 1;
      continue;
    }
    if (/[A-Za-z_$]/.test(c)) {
      const m = /^[A-Za-z_$][A-Za-z0-9_$]*/.exec(src.slice(i))!;
      out.push({ t: "id", v: m[0] });
      i += m[0].length;
      continue;
    }
    const op = OPS.find((o) => src.startsWith(o, i));
    if (!op) throw new ExprError(`Unexpected character '${c}' in: ${src}`);
    out.push({ t: "op", v: op });
    i += op.length;
  }
  return out;
}

export type Node =
  | { k: "lit"; v: unknown }
  | { k: "path"; parts: string[] }
  | { k: "arr"; items: Node[] }
  | { k: "un"; op: string; a: Node }
  | { k: "bin"; op: string; a: Node; b: Node }
  | { k: "cond"; c: Node; a: Node; b: Node };

const PREC: Record<string, number> = {
  "||": 1,
  "&&": 2,
  "==": 3,
  "!=": 3,
  "<": 4,
  "<=": 4,
  ">": 4,
  ">=": 4,
  in: 4,
  "+": 5,
  "-": 5,
  "*": 6,
  "/": 6,
};

export function parse(src: string): Node {
  const toks = tokenize(src);
  let p = 0;
  const peek = () => toks[p];
  const isOp = (v: string) => {
    const t = toks[p];
    return !!t && (t.t === "op" || t.t === "id") && t.v === v;
  };
  const expect = (v: string) => {
    if (!isOp(v)) throw new ExprError(`Expected '${v}' in: ${src}`);
    p++;
  };

  function primary(): Node {
    const t = toks[p++];
    if (!t) throw new ExprError(`Unexpected end of expression: ${src}`);
    if (t.t === "num" || t.t === "str") return { k: "lit", v: t.v };
    if (t.t === "op" && (t.v === "!" || t.v === "-")) return { k: "un", op: t.v, a: primary() };
    if (t.t === "op" && t.v === "(") {
      const e = expr(0);
      expect(")");
      return e;
    }
    if (t.t === "op" && t.v === "[") {
      const items: Node[] = [];
      if (!isOp("]")) {
        do items.push(expr(0));
        while (isOp(",") && ++p);
      }
      expect("]");
      return { k: "arr", items };
    }
    if (t.t === "id") {
      if (t.v === "true") return { k: "lit", v: true };
      if (t.v === "false") return { k: "lit", v: false };
      if (t.v === "null") return { k: "lit", v: null };
      const parts = [t.v];
      while (isOp(".")) {
        p++;
        const n = toks[p++];
        if (!n || n.t !== "id") throw new ExprError(`Expected property name in: ${src}`);
        parts.push(n.v);
      }
      return { k: "path", parts };
    }
    throw new ExprError(`Unexpected token '${t.v}' in: ${src}`);
  }

  function expr(minPrec: number): Node {
    let left = primary();
    for (;;) {
      const t = peek();
      if (!t) break;
      if (t.t === "op" && t.v === "?" && minPrec === 0) {
        p++;
        const a = expr(0);
        expect(":");
        const b = expr(0);
        left = { k: "cond", c: left, a, b };
        continue;
      }
      const prec = (t.t === "op" || t.t === "id") ? PREC[t.v] : undefined;
      if (prec === undefined || prec < minPrec) break;
      p++;
      const right = expr(prec + 1);
      left = { k: "bin", op: String(t.v), a: left, b: right };
    }
    return left;
  }

  const ast = expr(0);
  if (p !== toks.length) throw new ExprError(`Unexpected '${(toks[p] as Token).v}' in: ${src}`);
  return ast;
}

const BLOCKED = new Set(["__proto__", "prototype", "constructor"]);

function lookup(scope: Scope, parts: string[]): unknown {
  let cur: unknown = scope;
  for (const part of parts) {
    if (BLOCKED.has(part)) return undefined;
    if (cur === null || typeof cur !== "object") return undefined;
    if (!Object.prototype.hasOwnProperty.call(cur, part)) return undefined;
    cur = (cur as Record<string, unknown>)[part];
  }
  return cur;
}

function evalNode(n: Node, scope: Scope): unknown {
  switch (n.k) {
    case "lit":
      return n.v;
    case "path":
      return lookup(scope, n.parts);
    case "arr":
      return n.items.map((i) => evalNode(i, scope));
    case "un": {
      const a = evalNode(n.a, scope);
      return n.op === "!" ? !a : -Number(a);
    }
    case "cond":
      return evalNode(n.c, scope) ? evalNode(n.a, scope) : evalNode(n.b, scope);
    case "bin": {
      if (n.op === "&&") return evalNode(n.a, scope) && evalNode(n.b, scope);
      if (n.op === "||") return evalNode(n.a, scope) || evalNode(n.b, scope);
      const a = evalNode(n.a, scope) as any;
      const b = evalNode(n.b, scope) as any;
      switch (n.op) {
        case "==":
          return a === b;
        case "!=":
          return a !== b;
        case "<":
          return a < b;
        case "<=":
          return a <= b;
        case ">":
          return a > b;
        case ">=":
          return a >= b;
        case "in":
          return Array.isArray(b) ? b.includes(a) : typeof b === "string" ? b.includes(String(a)) : false;
        case "+":
          return typeof a === "number" && typeof b === "number" ? a + b : `${a ?? ""}${b ?? ""}`;
        case "-":
          return Number(a) - Number(b);
        case "*":
          return Number(a) * Number(b);
        case "/":
          return Number(a) / Number(b);
      }
    }
  }
  throw new ExprError("Invalid expression");
}

const cache = new Map<string, Node>();

export function evaluate(src: string, scope: Scope): unknown {
  let ast = cache.get(src);
  if (!ast) {
    ast = parse(src);
    if (cache.size > 1000) cache.clear();
    cache.set(src, ast);
  }
  return evalNode(ast, scope);
}

/** Evaluates a condition; an empty/absent condition is true. */
export function test(src: string | undefined, scope: Scope): boolean {
  if (src === undefined || src.trim() === "") return true;
  return Boolean(evaluate(src, scope));
}
