import { useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { Trash2 } from "lucide-react"
import { getTypeOptions } from "@/api/generated/@tanstack/react-query.gen"
import { addDependency, removeDependency, removeResource, renameResource, resources, withResources, type Resource } from "@/components/designer/blueprint"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { useDesigner } from "@/stores/designer"

/** Inspector edits the selected resource: its id, condition, locator params and values. */
export function Inspector({ repository, resource }: { repository: string; resource: Resource }) {
  const draft = useDesigner((s) => s.draft)
  const edit = useDesigner((s) => s.edit)
  const select = useDesigner((s) => s.select)
  const typeRepo = resource.type.repository ?? repository
  const { data: type } = useQuery(getTypeOptions({ path: { repository: typeRepo, name: resource.type.name } }))
  const others = resources(draft).filter((r) => r.id !== resource.id)
  const update = (patch: Partial<Resource>) => edit((d) => withResources(d, resources(d).map((r) => (r.id === resource.id ? { ...r, ...patch } : r))))

  return (
    <div className="grid gap-4" aria-label={`Resource ${resource.id}`} role="group">
      <IdField
        id={resource.id}
        onRename={(to) => {
          const ok = edit((d) => renameResource(d, resource.id, to))
          if (ok) select(to)
          return ok
        }}
      />
      <p className="text-muted-foreground text-xs">
        {type?.title ?? resource.type.name} · {typeRepo}
      </p>
      <div className="grid gap-2">
        <Label htmlFor="resource-when">Created when</Label>
        <Input
          id="resource-when"
          className="font-mono text-xs"
          placeholder="always"
          value={resource.when ?? ""}
          onChange={(e) => update({ when: e.target.value || undefined })}
        />
        <p className="text-muted-foreground text-xs">A CEL condition on the inputs, such as inputs.withDatabase.</p>
      </div>
      {(type?.params ?? []).length > 0 && (
        <fieldset className="grid gap-3">
          <legend className="mb-1 text-sm font-medium">Where</legend>
          {(type?.params ?? []).map((p) => (
            <div key={p.name} className="grid gap-1.5">
              <Label htmlFor={`param-${p.name}`}>{p.title ?? p.name}</Label>
              <Input
                id={`param-${p.name}`}
                className="font-mono text-xs"
                placeholder={"${{ inputs.… }}"}
                value={paramText(resource.params?.[p.name])}
                onChange={(e) => {
                  const params = { ...resource.params, [p.name]: e.target.value }
                  if (e.target.value === "") delete params[p.name]
                  update({ params })
                }}
              />
            </div>
          ))}
        </fieldset>
      )}
      <ValuesField key={resource.id} values={resource.values ?? {}} onChange={(values) => update({ values })} />
      {others.length > 0 && (
        <fieldset className="grid gap-2">
          <legend className="mb-1 text-sm font-medium">Depends on</legend>
          {others.map((o) => {
            const on = resource.dependsOn?.includes(o.id) ?? false
            return (
              <div key={o.id} className="flex items-center gap-2">
                <Checkbox
                  id={`depends-${o.id}`}
                  checked={on}
                  onCheckedChange={(c) => edit((d) => (c ? addDependency(d, resource.id, o.id) : removeDependency(d, resource.id, o.id)))}
                />
                <Label htmlFor={`depends-${o.id}`} className="font-normal">
                  {o.id}
                </Label>
              </div>
            )
          })}
        </fieldset>
      )}
      <Button
        type="button"
        variant="outline"
        className="text-destructive w-fit"
        onClick={() => {
          edit((d) => removeResource(d, resource.id))
          select(null)
        }}
      >
        <Trash2 /> Remove {resource.id}
      </Button>
    </div>
  )
}

const paramText = (v: unknown) => (v === undefined ? "" : typeof v === "string" ? v : JSON.stringify(v))

function IdField({ id, onRename }: { id: string; onRename: (to: string) => boolean }) {
  const [value, setValue] = useState(id)
  const [error, setError] = useState<string>()
  const commit = () => {
    if (value === id) return
    if (onRename(value)) setError(undefined)
    else setError("Use lowercase letters, digits and '-', starting with a letter, not used by another resource.")
  }
  return (
    <div className="grid gap-2">
      <Label htmlFor="resource-id">Resource id</Label>
      <Input
        id="resource-id"
        value={value}
        aria-invalid={error ? true : undefined}
        onChange={(e) => setValue(e.target.value)}
        onBlur={commit}
        onKeyDown={(e) => e.key === "Enter" && commit()}
      />
      {error && <p className="text-destructive text-xs">{error}</p>}
    </div>
  )
}

/** ValuesField edits a resource's values as JSON: they may hold ${{ }} expressions anywhere. */
function ValuesField({ values, onChange }: { values: Record<string, unknown>; onChange: (v: Record<string, unknown>) => void }) {
  const [text, setText] = useState(() => JSON.stringify(values, null, 2))
  const [error, setError] = useState<string>()
  const commit = () => {
    try {
      const v: unknown = JSON.parse(text || "{}")
      if (v === null || typeof v !== "object" || Array.isArray(v)) throw new Error("must be an object")
      setError(undefined)
      onChange(v as Record<string, unknown>)
    } catch (e) {
      setError(`Not valid: ${e instanceof Error ? e.message : String(e)}`)
    }
  }
  return (
    <div className="grid gap-2">
      <Label htmlFor="resource-values">Values</Label>
      <Textarea id="resource-values" rows={8} className="font-mono text-xs" value={text} aria-invalid={error ? true : undefined} onChange={(e) => setText(e.target.value)} onBlur={commit} />
      <p className="text-muted-foreground text-xs">JSON, checked against the type's schema when you publish. Strings may hold {"${{ }}"} expressions.</p>
      {error && <p className="text-destructive text-xs">{error}</p>}
    </div>
  )
}
