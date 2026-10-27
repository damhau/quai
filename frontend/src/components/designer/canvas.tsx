import { useMemo, useState } from "react"
import { applyEdgeChanges, applyNodeChanges, Background, Controls, Handle, Position, ReactFlow, type Connection, type Edge, type Node, type NodeProps } from "@xyflow/react"
import "@xyflow/react/dist/style.css"
import { addDependency, layout, removeDependency, resources, type Resource } from "@/components/designer/blueprint"
import { Badge } from "@/components/ui/badge"
import { cn } from "@/lib/utils"
import { useDesigner } from "@/stores/designer"

type ResourceNode = Node<{ resource: Resource; title: string }, "resource">

function ResourceCard({ data, selected }: NodeProps<ResourceNode>) {
  const r = data.resource
  return (
    <div className={cn("bg-card w-56 rounded-lg border px-3 py-2 shadow-sm", selected && "ring-ring ring-2")}>
      <Handle type="target" position={Position.Left} />
      <div className="text-sm font-medium">{r.id}</div>
      <div className="text-muted-foreground truncate text-xs">{data.title}</div>
      {r.when && (
        <Badge variant="outline" className="mt-1 max-w-full truncate font-mono text-[10px]">
          when {r.when}
        </Badge>
      )}
      <Handle type="source" position={Position.Right} />
    </div>
  )
}

const nodeTypes = { resource: ResourceCard }

/**
 * Canvas draws the blueprint's resources, each after what it depends on. Drawing an edge from A
 * to B makes B depend on A; deleting an edge removes the dependency.
 */
export function Canvas({ titles }: { titles: Map<string, string> }) {
  const draft = useDesigner((s) => s.draft)
  const selected = useDesigner((s) => s.selected)
  const edit = useDesigner((s) => s.edit)
  const select = useDesigner((s) => s.select)
  const rs = resources(draft)
  const positions = useMemo(() => layout(rs), [rs])
  // React Flow keeps its own copy, to measure, select and move nodes. When the draft changes,
  // the copy is laid out again during the render, keeping the sizes already measured.
  const toNodes = (old: ResourceNode[]): ResourceNode[] =>
    rs.map((r) => ({
      id: r.id,
      type: "resource",
      position: positions.get(r.id) ?? { x: 0, y: 0 },
      selected: r.id === selected,
      measured: old.find((o) => o.id === r.id)?.measured,
      data: { resource: r, title: titles.get(r.type.name) ?? r.type.name },
    }))
  const toEdges = (): Edge[] => rs.flatMap((r) => (r.dependsOn ?? []).map((on) => ({ id: `${on}->${r.id}`, source: on, target: r.id })))
  const [nodes, setNodes] = useState<ResourceNode[]>(() => toNodes([]))
  const [edges, setEdges] = useState<Edge[]>(toEdges)
  const [shown, setShown] = useState({ rs, selected, titles })
  if (shown.rs !== rs || shown.selected !== selected || shown.titles !== titles) {
    setShown({ rs, selected, titles })
    setNodes(toNodes(nodes))
    setEdges(toEdges())
  }
  return (
    <div className="bg-muted/30 h-[480px] rounded-lg border" aria-label="Canvas">
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        fitView
        onNodesChange={(c) => setNodes((n) => applyNodeChanges(c, n))}
        onEdgesChange={(c) => setEdges((e) => applyEdgeChanges(c, e))}
        onNodeClick={(_, n) => select(n.id)}
        onPaneClick={() => select(null)}
        onConnect={(c: Connection) => edit((d) => addDependency(d, c.target, c.source))}
        onEdgesDelete={(es) => es.forEach((e) => edit((d) => removeDependency(d, e.target, e.source)))}
        proOptions={{ hideAttribution: true }}
      >
        <Background />
        <Controls showInteractive={false} />
      </ReactFlow>
    </div>
  )
}
