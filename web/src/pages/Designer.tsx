import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import {
  Background,
  Controls,
  Handle,
  MiniMap,
  Position,
  ReactFlow,
  ReactFlowProvider,
  useReactFlow,
  type Connection as FlowConnection,
  type Edge,
  type Node,
  type NodeProps,
  type NodeChange,
  type EdgeChange,
  applyNodeChanges,
  applyEdgeChanges,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import "./designer.css";
import { parse as parseYaml } from "yaml";
import {
  defaultsFor,
  hasBinding,
  renderDeployment,
  serializeBlueprint,
  validateBlueprint,
  validateInputs,
  type Blueprint,
  type InputDef,
  type ResourceNode,
} from "@quai/core";
import { api, useApi, type ComponentView } from "../api";
import { Icon } from "../components/Icon";
import { ChangesView, ErrorNote } from "../components/Bits";
import { DynamicForm, optionList } from "../components/DynamicForm";
import { useMe } from "../App";

const blank = (): Blueprint => ({
  apiVersion: "quai.dev/v1alpha1",
  kind: "Blueprint",
  metadata: { name: "new-blueprint", title: "New blueprint", description: "", icon: "layers", category: "Applications", version: "0.1.0" },
  spec: {
    inputs: [{ name: "environment", title: "Environment", type: "enum", options: ["dev", "test", "prod"], default: "dev", required: true }],
    resources: [],
    connections: [],
    approval: { rules: [{ name: "production", when: "inputs.environment == 'prod'", approvers: ["group:platform-admins"] }] },
    actions: [],
  },
});

type ResData = { res: ResourceNode; component?: ComponentView; selected?: boolean };

function ResourceCard({ data, selected }: NodeProps<Node<ResData>>) {
  const c = data.component;
  const bound = Object.values(data.res.properties ?? {}).filter(hasBinding).length;
  return (
    <div className={`res-node ${selected ? "sel" : ""}`} style={{ "--accent": c?.color ?? "#94a3b8" } as React.CSSProperties}>
      <Handle type="target" position={Position.Left} />
      <div className="res-head">
        <span className="res-icon">
          <Icon name={c?.icon} size={14} />
        </span>
        <div>
          <div className="res-title">{data.res.title ?? data.res.id}</div>
          <div className="res-type">{c?.title ?? data.res.type}</div>
        </div>
      </div>
      <div className="res-foot">
        <code>{data.res.id}</code>
        {bound > 0 && <span title="Properties bound to form inputs">⟶ {bound} bound</span>}
        {data.res.when && <span title={`Only when ${data.res.when}`}>conditional</span>}
      </div>
      <Handle type="source" position={Position.Right} />
    </div>
  );
}

const nodeTypes = { resource: ResourceCard };

/** Right of the rightmost node, so a click-added component never covers another. */
function nextFreeSpot(resources: ResourceNode[]) {
  if (!resources.length) return { x: 80, y: 120 };
  const right = resources.reduce((a, r) => ((r.position?.x ?? 0) > (a.position?.x ?? 0) ? r : a));
  return { x: (right.position?.x ?? 0) + 280, y: right.position?.y ?? 120 };
}

export function Designer() {
  return (
    <ReactFlowProvider>
      <DesignerInner />
    </ReactFlowProvider>
  );
}

function DesignerInner() {
  const { name } = useParams();
  const me = useMe();
  const nav = useNavigate();
  const { data: components } = useApi<ComponentView[]>("/components");
  const [bp, setBp] = useState<Blueprint>();
  const [selected, setSelected] = useState<string>();
  const [panel, setPanel] = useState<"canvas" | "inputs" | "policy" | "yaml" | "preview">("canvas");
  const [yamlText, setYamlText] = useState("");
  const [yamlError, setYamlError] = useState<string>();
  const [saveState, setSaveState] = useState<{ ok?: string; error?: Error }>({});
  const [dirty, setDirty] = useState(false);
  const flow = useReactFlow();

  useEffect(() => {
    if (!name) {
      setBp(blank());
      return;
    }
    api<Blueprint>(`/blueprints/${name}`).then(setBp, (e) => setSaveState({ error: e }));
  }, [name]);

  const update = useCallback((fn: (b: Blueprint) => Blueprint) => {
    setBp((b) => (b ? fn(structuredClone(b)) : b));
    setDirty(true);
  }, []);

  // ---- canvas <-> model. The blueprint is the single source of truth; the canvas is a view of it.
  const compByType = useMemo(() => new Map((components ?? []).map((c) => [c.type, c])), [components]);
  const nodes: Node<ResData>[] = useMemo(
    () =>
      (bp?.spec.resources ?? []).map((r, i) => ({
        id: r.id,
        type: "resource",
        position: r.position ?? { x: 80 + i * 260, y: 120 },
        data: { res: r, component: compByType.get(r.type) },
        selected: r.id === selected,
      })),
    [bp, compByType, selected],
  );
  const edges: Edge[] = useMemo(
    () => (bp?.spec.connections ?? []).map((c) => ({ id: `${c.from}->${c.to}`, source: c.from, target: c.to, animated: true, label: compByType.get(bp!.spec.resources.find((r) => r.id === c.to)?.type ?? "")?.publishesSecret ? "secret" : undefined })),
    [bp, compByType],
  );

  const onNodesChange = (changes: NodeChange<Node<ResData>>[]) => {
    const moved = applyNodeChanges(changes, nodes);
    for (const ch of changes) if (ch.type === "select" && ch.selected) setSelected(ch.id);
    const removed = changes.filter((c) => c.type === "remove").map((c) => (c as { id: string }).id);
    const positionsChanged = changes.some((c) => c.type === "position" && c.position);
    if (!positionsChanged && !removed.length) return;
    update((b) => {
      b.spec.resources = b.spec.resources
        .filter((r) => !removed.includes(r.id))
        .map((r) => {
          const n = moved.find((m) => m.id === r.id);
          return n ? { ...r, position: { x: Math.round(n.position.x), y: Math.round(n.position.y) } } : r;
        });
      b.spec.connections = (b.spec.connections ?? []).filter((c) => !removed.includes(c.from) && !removed.includes(c.to));
      return b;
    });
    if (removed.includes(selected ?? "")) setSelected(undefined);
  };

  const onEdgesChange = (changes: EdgeChange[]) => {
    const kept = applyEdgeChanges(changes, edges);
    if (kept.length === edges.length) return;
    update((b) => {
      b.spec.connections = kept.map((e) => ({ from: e.source, to: e.target }));
      return b;
    });
  };

  const onConnect = (c: FlowConnection) => {
    if (!c.source || !c.target || c.source === c.target) return;
    update((b) => {
      b.spec.connections = [...(b.spec.connections ?? []).filter((x) => !(x.from === c.source && x.to === c.target)), { from: c.source!, to: c.target! }];
      return b;
    });
  };

  const addResource = (type: string, position?: { x: number; y: number }) => {
    const comp = compByType.get(type);
    if (!comp || !bp) return;
    const base = type.split(".").pop()!.replace(/[^a-z0-9]/g, "").slice(0, 12) || "res";
    let id = base;
    for (let i = 2; bp.spec.resources.some((r) => r.id === id); i++) id = `${base}${i}`;
    const props = defaultsFor(comp.properties);
    const res: ResourceNode = { id, type, properties: type === "raw" ? { manifest: { apiVersion: "v1", kind: "ConfigMap", metadata: { name: "${{ deployment.name }}-config" }, data: {} } } : props, position: position ?? nextFreeSpot(bp.spec.resources) };
    update((b) => {
      b.spec.resources.push(res);
      return b;
    });
    setSelected(id);
    if (!position) setTimeout(() => flow.fitView({ padding: 0.15, maxZoom: 1.2, duration: 200 }), 50);
  };

  const onDrop = (e: React.DragEvent) => {
    e.preventDefault();
    const type = e.dataTransfer.getData("application/quai-component");
    if (type) addResource(type, flow.screenToFlowPosition({ x: e.clientX, y: e.clientY }));
  };

  // ---- YAML view: edits round-trip into the model, so the code and the diagram never drift.
  useEffect(() => {
    if (panel === "yaml" && bp) {
      setYamlText(serializeBlueprint(bp));
      setYamlError(undefined);
    }
  }, [panel]); // eslint-disable-line react-hooks/exhaustive-deps

  const onYaml = (text: string) => {
    setYamlText(text);
    try {
      const parsed = parseYaml(text);
      const errs = validateBlueprint(parsed);
      if (errs.length) return setYamlError(errs.join("\n"));
      setYamlError(undefined);
      setBp(parsed);
      setDirty(true);
    } catch (e) {
      setYamlError((e as Error).message);
    }
  };

  const problems = useMemo(() => (bp ? validateBlueprint(bp) : []), [bp]);

  const save = async () => {
    if (!bp) return;
    setSaveState({});
    try {
      if (!name) {
        const exists = await api(`/blueprints/${bp.metadata.name}`).then(() => true, () => false);
        if (exists) throw new Error(`A blueprint named '${bp.metadata.name}' already exists; pick another name`);
      }
      const r = await api<{ sha: string; noop: boolean }>(`/blueprints/${bp.metadata.name}`, { method: "PUT", body: bp });
      setSaveState({ ok: r.noop ? "No changes to commit." : `Committed ${r.sha.slice(0, 7)} to the catalog.` });
      setDirty(false);
      if (!name) nav(`/designer/${bp.metadata.name}`, { replace: true });
    } catch (e) {
      setSaveState({ error: e as Error });
    }
  };

  if (!me.admin)
    return (
      <div className="page">
        <div className="note warn">The designer is for platform admins. Switch to Alice in the sidebar to try it.</div>
      </div>
    );
  if (!bp || !components) return <div className="page muted">{saveState.error ? <ErrorNote error={saveState.error} /> : "Loading…"}</div>;

  const sel = bp.spec.resources.find((r) => r.id === selected);
  const grouped = components.reduce<Record<string, ComponentView[]>>((acc, c) => ((acc[c.category] ??= []).push(c), acc), {});

  return (
    <div className="designer">
      <div className="designer-bar">
        <div className="row">
          <span className="bp-icon">
            <Icon name={bp.metadata.icon} size={18} />
          </span>
          <input className="title-input" value={bp.metadata.title} onChange={(e) => update((b) => ((b.metadata.title = e.target.value), b))} aria-label="Blueprint title" />
          <code className="muted">{bp.metadata.name}</code>
          <span className="tag">v{bp.metadata.version}</span>
          {dirty && <span className="tag warn">unsaved</span>}
        </div>
        <div className="tabs compact" role="tablist">
          {(["canvas", "inputs", "policy", "yaml", "preview"] as const).map((t) => (
            <button key={t} role="tab" aria-selected={panel === t} className={panel === t ? "on" : ""} onClick={() => setPanel(t)}>
              {t === "canvas" ? "Canvas" : t === "inputs" ? `Form (${bp.spec.inputs.length})` : t === "policy" ? "Approvals & actions" : t === "yaml" ? "YAML" : "Preview"}
            </button>
          ))}
        </div>
        <div className="row">
          {problems.length > 0 && (
            <span className="badge bad" title={problems.join("\n")}>
              {problems.length} problem{problems.length > 1 ? "s" : ""}
            </span>
          )}
          <button className="btn primary" onClick={save} disabled={problems.length > 0}>
            <Icon name="save" /> Commit to catalog
          </button>
        </div>
      </div>
      {(saveState.ok || saveState.error) && (
        <div className="designer-msg">
          {saveState.ok && (
            <div className="note ok">
              <Icon name="check" size={14} /> {saveState.ok}
            </div>
          )}
          <ErrorNote error={saveState.error} />
        </div>
      )}

      {panel === "canvas" && (
        <div className="designer-body">
          <aside className="palette">
            <div className="small muted">Drag onto the canvas</div>
            {Object.entries(grouped).map(([cat, comps]) => (
              <div key={cat}>
                <div className="nav-section">{cat}</div>
                {comps.map((c) => (
                  <div
                    key={c.type}
                    className="palette-item"
                    draggable
                    onDragStart={(e) => e.dataTransfer.setData("application/quai-component", c.type)}
                    onDoubleClick={() => addResource(c.type)}
                    title={c.description}
                    style={{ "--accent": c.color } as React.CSSProperties}
                  >
                    <span className="res-icon">
                      <Icon name={c.icon} size={14} />
                    </span>
                    {c.title}
                    <button className="icon-btn tiny" onClick={() => addResource(c.type)} aria-label={`Add ${c.title}`}>
                      <Icon name="plus" size={12} />
                    </button>
                  </div>
                ))}
              </div>
            ))}
          </aside>
          <div className="canvas" onDragOver={(e) => e.preventDefault()} onDrop={onDrop}>
            <ReactFlow
              nodes={nodes}
              edges={edges}
              nodeTypes={nodeTypes}
              onNodesChange={onNodesChange}
              onEdgesChange={onEdgesChange}
              onConnect={onConnect}
              onPaneClick={() => setSelected(undefined)}
              fitView
              fitViewOptions={{ padding: 0.15, maxZoom: 1.2 }}
              deleteKeyCode={["Backspace", "Delete"]}
              proOptions={{ hideAttribution: true }}
            >
              <Background gap={20} size={1} />
              <Controls />
              <MiniMap pannable zoomable />
            </ReactFlow>
            {!bp.spec.resources.length && <div className="canvas-hint">Drag components here, then wire them together. An edge passes the target's connection secret to the source.</div>}
          </div>
          <aside className="inspector">
            {sel ? (
              <Inspector key={sel.id} res={sel} component={compByType.get(sel.type)} inputs={bp.spec.inputs} onChange={(r) => update((b) => ((b.spec.resources = b.spec.resources.map((x) => (x.id === sel.id ? r : x))), b))} onRename={(id) => {
                if (!/^[a-z][a-z0-9-]{0,30}$/.test(id) || bp.spec.resources.some((r) => r.id === id)) return false;
                update((b) => {
                  b.spec.resources = b.spec.resources.map((x) => (x.id === sel.id ? { ...x, id } : x));
                  b.spec.connections = (b.spec.connections ?? []).map((c) => ({ from: c.from === sel.id ? id : c.from, to: c.to === sel.id ? id : c.to }));
                  return b;
                });
                setSelected(id);
                return true;
              }} />
            ) : (
              <MetadataPanel bp={bp} onChange={update} isNew={!name} />
            )}
          </aside>
        </div>
      )}
      {panel === "inputs" && <InputsEditor bp={bp} onChange={update} />}
      {panel === "policy" && <PolicyEditor bp={bp} onChange={update} />}
      {panel === "yaml" && (
        <div className="yaml-pane">
          <textarea value={yamlText} onChange={(e) => onYaml(e.target.value)} spellCheck={false} aria-label="Blueprint YAML" />
          <div className="yaml-status">{yamlError ? <pre className="note bad">{yamlError}</pre> : <div className="note ok">Valid — canvas updated.</div>}</div>
        </div>
      )}
      {panel === "preview" && <Preview bp={bp} />}
    </div>
  );
}

function MetadataPanel({ bp, onChange, isNew }: { bp: Blueprint; onChange: (fn: (b: Blueprint) => Blueprint) => void; isNew: boolean }) {
  const m = bp.metadata;
  const set = (k: keyof Blueprint["metadata"], v: string) => onChange((b) => ((b.metadata[k] = v as never), b));
  return (
    <div className="form">
      <h3>Blueprint</h3>
      <div className="field">
        <label htmlFor="m-name">Name (id)</label>
        <input id="m-name" value={m.name} disabled={!isNew} onChange={(e) => set("name", e.target.value.toLowerCase())} />
      </div>
      <div className="field">
        <label htmlFor="m-desc">Description</label>
        <textarea id="m-desc" rows={3} value={m.description ?? ""} onChange={(e) => set("description", e.target.value)} />
      </div>
      <div className="field">
        <label htmlFor="m-cat">Category</label>
        <input id="m-cat" value={m.category ?? ""} onChange={(e) => set("category", e.target.value)} />
      </div>
      <div className="field">
        <label htmlFor="m-ver">Version</label>
        <input id="m-ver" value={m.version ?? ""} onChange={(e) => set("version", e.target.value)} />
      </div>
      <div className="field">
        <label htmlFor="m-icon">Icon</label>
        <select id="m-icon" value={m.icon} onChange={(e) => set("icon", e.target.value)}>
          {["layers", "server", "database", "bucket", "rocket", "terraform", "box", "network", "code"].map((i) => (
            <option key={i}>{i}</option>
          ))}
        </select>
      </div>
      <p className="small muted">Select a resource on the canvas to edit it. Bind properties to form inputs with <code>{"${{ inputs.name }}"}</code>.</p>
    </div>
  );
}

function Inspector({ res, component, inputs, onChange, onRename }: { res: ResourceNode; component?: ComponentView; inputs: InputDef[]; onChange: (r: ResourceNode) => void; onRename: (id: string) => boolean }) {
  const [id, setId] = useState(res.id);
  const props = res.properties ?? {};
  const setProp = (k: string, v: unknown) => onChange({ ...res, properties: { ...props, [k]: v } });
  const bind = (k: string, input: string) => setProp(k, input ? `\${{ inputs.${input} }}` : component?.properties.find((p) => p.name === k)?.default);

  return (
    <div className="form">
      <h3 className="row">
        <Icon name={component?.icon} /> {component?.title}
      </h3>
      <p className="small muted">{component?.description}</p>
      <div className="field">
        <label htmlFor="r-id">Resource id</label>
        <input id="r-id" value={id} onChange={(e) => setId(e.target.value)} onBlur={() => !onRename(id) && setId(res.id)} />
      </div>
      <div className="field">
        <label htmlFor="r-when">Only when (expression)</label>
        <input id="r-when" value={res.when ?? ""} placeholder="e.g. inputs.withBucket" onChange={(e) => onChange({ ...res, when: e.target.value || undefined })} spellCheck={false} />
      </div>
      {component?.properties.map((p) => {
        const v = props[p.name];
        const binding = typeof v === "string" ? /^\$\{\{\s*inputs\.([A-Za-z0-9_]+)\s*\}\}$/.exec(v)?.[1] : undefined;
        const expr = hasBinding(v) && !binding;
        return (
          <div className="field prop" key={p.name}>
            <label>
              {p.title ?? p.name}
              <select className="bind" value={binding ?? (expr ? "__expr" : "")} onChange={(e) => (e.target.value === "__expr" ? setProp(p.name, `\${{ ${binding ? `inputs.${binding}` : "inputs.environment == 'prod'"} }}`) : bind(p.name, e.target.value))} aria-label={`Bind ${p.title ?? p.name}`}>
                <option value="">fixed value</option>
                {inputs.map((i) => (
                  <option key={i.name} value={i.name}>
                    ⟶ inputs.{i.name}
                  </option>
                ))}
                <option value="__expr">expression…</option>
              </select>
            </label>
            {binding ? (
              <div className="bound">
                <Icon name="zap" size={12} /> from form input <b>{binding}</b>
              </div>
            ) : expr ? (
              <input value={String(v)} onChange={(e) => setProp(p.name, e.target.value)} spellCheck={false} className="mono" />
            ) : p.type === "boolean" ? (
              <label className="switch">
                <input type="checkbox" checked={v === true} onChange={(e) => setProp(p.name, e.target.checked)} />
                <span className="track" />
              </label>
            ) : p.type === "enum" ? (
              <select value={String(v ?? "")} onChange={(e) => setProp(p.name, e.target.value)}>
                {optionList(p).map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </select>
            ) : (
              <input
                type={p.type === "integer" || p.type === "number" ? "number" : "text"}
                value={v === undefined ? "" : String(v)}
                placeholder={p.default === undefined ? undefined : `default: ${p.default}`}
                onChange={(e) => setProp(p.name, p.type === "integer" || p.type === "number" ? (e.target.value === "" ? undefined : Number(e.target.value)) : e.target.value)}
              />
            )}
          </div>
        );
      })}
      {(res.type === "raw" || res.type === "terraform.module") && (
        <JsonField
          label={res.type === "raw" ? "Manifest (JSON or YAML)" : "Terraform variables (JSON or YAML)"}
          value={props[res.type === "raw" ? "manifest" : "vars"] ?? {}}
          onChange={(v) => setProp(res.type === "raw" ? "manifest" : "vars", v)}
        />
      )}
    </div>
  );
}

function JsonField({ label, value, onChange }: { label: string; value: unknown; onChange: (v: unknown) => void }) {
  const [text, setText] = useState(() => JSON.stringify(value, null, 2));
  const [err, setErr] = useState<string>();
  return (
    <div className={`field ${err ? "has-error" : ""}`}>
      <label>{label}</label>
      <textarea
        rows={10}
        className="mono"
        value={text}
        spellCheck={false}
        onChange={(e) => {
          setText(e.target.value);
          try {
            const v = parseYaml(e.target.value);
            if (!v || typeof v !== "object") throw new Error("Must be an object");
            setErr(undefined);
            onChange(v);
          } catch (x) {
            setErr((x as Error).message);
          }
        }}
      />
      {err && <div className="error">{err}</div>}
    </div>
  );
}

const INPUT_TYPES: InputDef["type"][] = ["string", "integer", "number", "boolean", "enum"];

function InputsEditor({ bp, onChange }: { bp: Blueprint; onChange: (fn: (b: Blueprint) => Blueprint) => void }) {
  const [values, setValues] = useState<Record<string, unknown>>(() => defaultsFor(bp.spec.inputs));
  const setInput = (i: number, patch: Partial<InputDef>) =>
    onChange((b) => {
      b.spec.inputs[i] = { ...b.spec.inputs[i], ...patch };
      for (const k of Object.keys(b.spec.inputs[i]) as (keyof InputDef)[]) if (b.spec.inputs[i][k] === "" || b.spec.inputs[i][k] === undefined) delete b.spec.inputs[i][k];
      return b;
    });
  const move = (i: number, d: number) =>
    onChange((b) => {
      const [x] = b.spec.inputs.splice(i, 1);
      b.spec.inputs.splice(Math.max(0, i + d), 0, x);
      return b;
    });
  const { errors } = validateInputs(bp.spec.inputs, values);

  return (
    <div className="designer-panel split">
      <div className="panel">
        <h2>Request form</h2>
        <p className="small muted">These become the self-service form. Bind them to resource properties on the canvas.</p>
        <table className="table compact inputs-table">
          <thead>
            <tr>
              <th>Name</th>
              <th>Label</th>
              <th>Type</th>
              <th>Default</th>
              <th>Options / range</th>
              <th>Visible if</th>
              <th>Req.</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {bp.spec.inputs.map((d, i) => (
              <tr key={i}>
                <td>
                  <input value={d.name} onChange={(e) => setInput(i, { name: e.target.value.replace(/[^A-Za-z0-9_]/g, "") })} aria-label="Input name" />
                </td>
                <td>
                  <input value={d.title ?? ""} onChange={(e) => setInput(i, { title: e.target.value })} aria-label="Input label" />
                </td>
                <td>
                  <select value={d.type} onChange={(e) => setInput(i, { type: e.target.value as InputDef["type"], options: e.target.value === "enum" ? d.options ?? ["a", "b"] : undefined })} aria-label="Input type">
                    {INPUT_TYPES.map((t) => (
                      <option key={t}>{t}</option>
                    ))}
                  </select>
                </td>
                <td>
                  <input
                    value={d.default === undefined ? "" : String(d.default)}
                    onChange={(e) => setInput(i, { default: e.target.value === "" ? undefined : d.type === "integer" || d.type === "number" ? Number(e.target.value) : d.type === "boolean" ? e.target.value === "true" : e.target.value })}
                    aria-label="Default"
                  />
                </td>
                <td>
                  {d.type === "enum" ? (
                    <input value={optionList(d).map((o) => o.value).join(", ")} onChange={(e) => setInput(i, { options: e.target.value.split(",").map((s) => s.trim()).filter(Boolean) })} aria-label="Options" />
                  ) : d.type === "integer" || d.type === "number" ? (
                    <div className="row">
                      <input className="xs" placeholder="min" value={d.min ?? ""} onChange={(e) => setInput(i, { min: e.target.value === "" ? undefined : Number(e.target.value) })} aria-label="Min" />
                      <input className="xs" placeholder="max" value={d.max ?? ""} onChange={(e) => setInput(i, { max: e.target.value === "" ? undefined : Number(e.target.value) })} aria-label="Max" />
                    </div>
                  ) : d.type === "string" ? (
                    <input placeholder="regex pattern" value={d.pattern ?? ""} onChange={(e) => setInput(i, { pattern: e.target.value })} aria-label="Pattern" className="mono" />
                  ) : null}
                </td>
                <td>
                  <input className="mono" placeholder="inputs.environment == 'prod'" value={d.visibleIf ?? ""} onChange={(e) => setInput(i, { visibleIf: e.target.value })} aria-label="Visible if" />
                </td>
                <td>
                  <input type="checkbox" checked={!!d.required} onChange={(e) => setInput(i, { required: e.target.checked || undefined })} aria-label="Required" />
                </td>
                <td className="nowrap">
                  <button className="icon-btn tiny" onClick={() => move(i, -1)} aria-label="Move up">
                    ↑
                  </button>
                  <button className="icon-btn tiny" onClick={() => onChange((b) => (b.spec.inputs.splice(i, 1), b))} aria-label="Remove input">
                    <Icon name="trash" size={12} />
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        <button className="btn sm" onClick={() => onChange((b) => (b.spec.inputs.push({ name: `field${b.spec.inputs.length + 1}`, title: "New field", type: "string" }), b))}>
          <Icon name="plus" size={14} /> Add input
        </button>
      </div>
      <div className="panel">
        <h2>Live form preview</h2>
        <DynamicForm inputs={bp.spec.inputs} values={values} onChange={setValues} errors={errors} />
      </div>
    </div>
  );
}

function PolicyEditor({ bp, onChange }: { bp: Blueprint; onChange: (fn: (b: Blueprint) => Blueprint) => void }) {
  const rules = bp.spec.approval?.rules ?? [];
  const setRule = (i: number, patch: Record<string, unknown>) =>
    onChange((b) => {
      b.spec.approval ??= { rules: [] };
      b.spec.approval.rules[i] = { ...b.spec.approval.rules[i], ...patch } as never;
      return b;
    });
  return (
    <div className="designer-panel split">
      <div className="panel">
        <h2>Approval rules</h2>
        <p className="small muted">Every rule whose condition matches needs its approvals. Requesters can never approve their own request; one denial rejects it.</p>
        {rules.map((r, i) => (
          <div className="rule-edit" key={i}>
            <div className="row">
              <input value={r.name} onChange={(e) => setRule(i, { name: e.target.value })} aria-label="Rule name" />
              <button className="icon-btn tiny" onClick={() => onChange((b) => (b.spec.approval!.rules.splice(i, 1), b))} aria-label="Remove rule">
                <Icon name="trash" size={12} />
              </button>
            </div>
            <label className="small">When</label>
            <input className="mono" value={r.when ?? ""} placeholder="always" onChange={(e) => setRule(i, { when: e.target.value || undefined })} />
            <label className="small">Approvers (comma separated, group:x or user:y)</label>
            <input value={r.approvers.join(", ")} onChange={(e) => setRule(i, { approvers: e.target.value.split(",").map((s) => s.trim()).filter(Boolean) })} />
            <label className="small">Approvals needed</label>
            <input type="number" min={1} value={r.min ?? 1} onChange={(e) => setRule(i, { min: Number(e.target.value) || 1 })} className="xs" />
          </div>
        ))}
        <button className="btn sm" onClick={() => onChange((b) => ((b.spec.approval ??= { rules: [] }).rules.push({ name: `rule-${rules.length + 1}`, approvers: ["group:platform-admins"] }), b))}>
          <Icon name="plus" size={14} /> Add rule
        </button>
      </div>
      <div className="panel">
        <h2>Day-2 actions</h2>
        <p className="small muted">
          <b>Edit configuration</b> and <b>Delete</b> are built in. Custom actions change selected inputs; edit their form and <code>set</code> mapping in the YAML tab.
        </p>
        {(bp.spec.actions ?? []).map((a, i) => (
          <div className="rule-edit" key={i}>
            <div className="row">
              <Icon name={a.icon} />
              <input value={a.title ?? a.name} onChange={(e) => onChange((b) => ((b.spec.actions![i].title = e.target.value), b))} aria-label="Action title" />
              <code className="small">{a.name}</code>
              <button className="icon-btn tiny" onClick={() => onChange((b) => (b.spec.actions!.splice(i, 1), b))} aria-label="Remove action">
                <Icon name="trash" size={12} />
              </button>
            </div>
            <div className="small muted">
              inputs: {(a.inputs ?? []).map((x) => x.name).join(", ") || "none"} · sets: {Object.keys(a.set ?? {}).join(", ") || "nothing"} · approval: {a.approval ? `${a.approval.rules.length} own rule(s)` : "blueprint rules"}
            </div>
          </div>
        ))}
        <button
          className="btn sm"
          onClick={() =>
            onChange((b) => {
              const first = b.spec.inputs[0];
              const n = `action-${(b.spec.actions?.length ?? 0) + 1}`;
              (b.spec.actions ??= []).push({ name: n, title: "New action", inputs: first ? [{ ...first }] : [], set: first ? { [first.name]: `\${{ action.${first.name} }}` } : {} });
              return b;
            })
          }
        >
          <Icon name="plus" size={14} /> Add action
        </button>
      </div>
    </div>
  );
}

function Preview({ bp }: { bp: Blueprint }) {
  const [values, setValues] = useState<Record<string, unknown>>(() => defaultsFor(bp.spec.inputs));
  const { values: clean, errors } = validateInputs(bp.spec.inputs, values);
  let result: { files: { path: string; content: string }[] } | undefined;
  let error: Error | undefined;
  try {
    result = renderDeployment(bp, { name: "preview", owner: "you", inputs: clean, createdAt: "today", updatedAt: "today" });
  } catch (e) {
    error = e as Error;
  }
  return (
    <div className="designer-panel split">
      <div className="panel">
        <h2>Try the form</h2>
        <DynamicForm inputs={bp.spec.inputs} values={values} onChange={setValues} errors={errors} />
      </div>
      <div className="panel">
        <h2>Rendered manifests</h2>
        <p className="small muted">Rendered in your browser with the same engine the server uses.</p>
        {error ? <ErrorNote error={error} /> : <ChangesView changes={(result?.files ?? []).map((f) => ({ path: f.path, op: "add", after: f.content }))} />}
      </div>
    </div>
  );
}

