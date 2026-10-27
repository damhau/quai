import { useState } from "react"
import { Plus, Trash2 } from "lucide-react"
import { metadata, properties, rules, withMetadata, withProperties, withRules, type InputType, type Property, type Rule } from "@/components/designer/blueprint"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Textarea } from "@/components/ui/textarea"
import { useDesigner } from "@/stores/designer"

const str = (v: unknown) => (typeof v === "string" ? v : "")

/** AboutEditor edits the blueprint's metadata; its name is fixed once published. */
export function AboutEditor({ isNew }: { isNew: boolean }) {
  const m = metadata(useDesigner((s) => s.draft))
  const edit = useDesigner((s) => s.edit)
  const set = (patch: Record<string, unknown>) => edit((d) => withMetadata(d, patch))
  const field = (key: string, label: string, props: { disabled?: boolean; placeholder?: string } = {}) => (
    <div className="grid gap-2">
      <Label htmlFor={`meta-${key}`}>{label}</Label>
      <Input id={`meta-${key}`} value={str(m[key])} onChange={(e) => set({ [key]: e.target.value })} {...props} />
    </div>
  )
  return (
    <div className="grid gap-4 sm:grid-cols-2">
      {field("name", "Name", { disabled: !isNew, placeholder: "web-and-db" })}
      {field("title", "Title")}
      <div className="grid gap-2 sm:col-span-2">
        <Label htmlFor="meta-description">Description</Label>
        <Textarea id="meta-description" rows={2} value={str(m.description)} onChange={(e) => set({ description: e.target.value })} />
      </div>
      {field("category", "Category", { placeholder: "Compute" })}
      {field("version", "Version", { placeholder: "1.0.0" })}
      {field("icon", "Icon", { placeholder: "server" })}
      <div className="grid gap-2">
        <Label htmlFor="meta-tags">Tags</Label>
        <Input
          id="meta-tags"
          placeholder="terraform, vm"
          value={Array.isArray(m.tags) ? (m.tags as string[]).join(", ") : ""}
          onChange={(e) => set({ tags: e.target.value.split(",").map((t) => t.trim()).filter(Boolean) })}
        />
      </div>
    </div>
  )
}

const types: InputType[] = ["string", "integer", "number", "boolean"]

/** InputsEditor edits the fields of the blueprint's form, which become its input schema. */
export function InputsEditor() {
  const ps = properties(useDesigner((s) => s.draft))
  const edit = useDesigner((s) => s.edit)
  const write = (next: Property[]) => edit((d) => withProperties(d, next))
  const set = (i: number, patch: Partial<Property>) => write(ps.map((p, j) => (j === i ? { ...p, ...patch } : p)))
  const add = () => {
    let name = "field"
    for (let n = 2; ps.some((p) => p.name === name); n++) name = `field${n}`
    write([...ps, { name, type: "string", title: "", required: false, default: "", options: "", schema: {} }])
  }
  return (
    <div className="grid gap-3">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead>Type</TableHead>
            <TableHead>Title</TableHead>
            <TableHead>Default</TableHead>
            <TableHead>Options</TableHead>
            <TableHead>Required</TableHead>
            <TableHead />
          </TableRow>
        </TableHeader>
        <TableBody>
          {ps.map((p, i) => (
            <TableRow key={i}>
              <TableCell>
                <NameCell key={p.name} index={i} name={p.name} taken={(n) => ps.some((q, j) => j !== i && q.name === n)} onRename={(name) => set(i, { name })} />
              </TableCell>
              <TableCell>
                <NativeSelect aria-label={`Field ${i + 1}: type`} value={p.type} onChange={(e) => set(i, { type: e.target.value as InputType })}>
                  {types.map((t) => (
                    <NativeSelectOption key={t} value={t}>
                      {t}
                    </NativeSelectOption>
                  ))}
                </NativeSelect>
              </TableCell>
              <TableCell>
                <Input aria-label={`Field ${i + 1}: title`} value={p.title} onChange={(e) => set(i, { title: e.target.value })} />
              </TableCell>
              <TableCell>
                <Input aria-label={`Field ${i + 1}: default`} value={p.default} onChange={(e) => set(i, { default: e.target.value })} />
              </TableCell>
              <TableCell>
                <Input aria-label={`Field ${i + 1}: options`} placeholder="a, b, c" value={p.options} onChange={(e) => set(i, { options: e.target.value })} />
              </TableCell>
              <TableCell>
                <Checkbox aria-label={`Field ${i + 1}: required`} checked={p.required} onCheckedChange={(c) => set(i, { required: c === true })} />
              </TableCell>
              <TableCell>
                <Button type="button" variant="ghost" size="icon" aria-label={`Remove field ${p.name}`} onClick={() => write(ps.filter((_, j) => j !== i))}>
                  <Trash2 />
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <Button type="button" variant="outline" size="sm" className="w-fit" onClick={add}>
        <Plus /> Add a field
      </Button>
      <p className="text-muted-foreground text-xs">Resources read these fields as inputs.&lt;name&gt;. Defaults and options take the field's type.</p>
    </div>
  )
}

/** NameCell edits a field's name, which takes effect once it is a valid name not used by another field. */
function NameCell({ index, name, taken, onRename }: { index: number; name: string; taken: (n: string) => boolean; onRename: (n: string) => void }) {
  const [value, setValue] = useState(name)
  const valid = /^[A-Za-z][A-Za-z0-9_]*$/.test(value) && !taken(value)
  return (
    <Input
      aria-label={`Field ${index + 1}: name`}
      aria-invalid={valid ? undefined : true}
      title={valid ? undefined : "A letter, then letters, digits or '_', not used by another field"}
      value={value}
      className="font-mono text-xs"
      onChange={(e) => setValue(e.target.value)}
      onBlur={() => (valid ? value !== name && onRename(value) : setValue(name))}
    />
  )
}

/** ApprovalEditor edits the blueprint's approval rules, which add to its types' and the repository's. */
export function ApprovalEditor() {
  const rs = rules(useDesigner((s) => s.draft))
  const edit = useDesigner((s) => s.edit)
  const write = (next: Rule[]) => edit((d) => withRules(d, next))
  const set = (i: number, patch: Partial<Rule>) => write(rs.map((r, j) => (j === i ? { ...r, ...patch } : r)))
  return (
    <div className="grid gap-3">
      {rs.map((r, i) => (
        <fieldset key={i} className="grid gap-3 rounded-lg border p-3 sm:grid-cols-[1fr_2fr_2fr_auto_auto] sm:items-end">
          <legend className="sr-only">Rule {i + 1}</legend>
          <div className="grid gap-1.5">
            <Label htmlFor={`rule-${i}-name`}>Name</Label>
            <Input id={`rule-${i}-name`} value={r.name} onChange={(e) => set(i, { name: e.target.value })} />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor={`rule-${i}-when`}>When</Label>
            <Input id={`rule-${i}-when`} className="font-mono text-xs" placeholder="always" value={r.when ?? ""} onChange={(e) => set(i, { when: e.target.value || undefined })} />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor={`rule-${i}-approvers`}>Approvers</Label>
            <Input
              id={`rule-${i}-approvers`}
              placeholder="group:finops, owners"
              value={(r.approvers ?? []).join(", ")}
              onChange={(e) => set(i, { approvers: e.target.value.split(",").map((a) => a.trim()).filter(Boolean) })}
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor={`rule-${i}-min`}>Approvals</Label>
            <Input id={`rule-${i}-min`} type="number" min={1} className="w-20" value={r.min ?? 1} onChange={(e) => set(i, { min: e.target.valueAsNumber || 1 })} />
          </div>
          <Button type="button" variant="ghost" size="icon" aria-label={`Remove rule ${r.name || i + 1}`} onClick={() => write(rs.filter((_, j) => j !== i))}>
            <Trash2 />
          </Button>
        </fieldset>
      ))}
      <Button type="button" variant="outline" size="sm" className="w-fit" onClick={() => write([...rs, { name: "", approvers: [], min: 1 }])}>
        <Plus /> Add a rule
      </Button>
      <p className="text-muted-foreground text-xs">They add to each type's rules and to the repository's floor rules; they never replace them.</p>
    </div>
  )
}
