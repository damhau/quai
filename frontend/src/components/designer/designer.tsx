import { useMemo, useState } from "react"
import { Link, useBlocker, useNavigate } from "@tanstack/react-router"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { AlertCircle, Plus, Upload } from "lucide-react"
import { toast } from "sonner"
import { getBlueprintQueryKey, listCatalogOptions, listCatalogQueryKey, publishBlueprintMutation } from "@/api/generated/@tanstack/react-query.gen"
import { addResource, metadata, resources, type Doc } from "@/components/designer/blueprint"
import { Canvas } from "@/components/designer/canvas"
import { AboutEditor, ApprovalEditor, InputsEditor } from "@/components/designer/editors"
import { Inspector } from "@/components/designer/inspector"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { isProblem, problemMessage } from "@/lib/problems"
import { cn } from "@/lib/utils"
import { createDesignerStore, DesignerContext, useDesigner } from "@/stores/designer"

/**
 * Designer edits a blueprint of a repository: its resources on a canvas, the fields of its form,
 * its approval rules and its metadata. Publishing commits the blueprint to the repository.
 */
export function Designer({ repository, initial, isNew }: { repository: string; initial: Doc; isNew: boolean }) {
  const [store] = useState(() => createDesignerStore(initial))
  return (
    <DesignerContext value={store}>
      <Editor repository={repository} isNew={isNew} />
    </DesignerContext>
  )
}

function Editor({ repository, isNew }: { repository: string; isNew: boolean }) {
  const draft = useDesigner((s) => s.draft)
  const selected = useDesigner((s) => s.selected)
  const dirty = useDesigner((s) => s.dirty)
  const edit = useDesigner((s) => s.edit)
  const select = useDesigner((s) => s.select)
  const published = useDesigner((s) => s.published)
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { data: catalog } = useQuery(listCatalogOptions())
  const types = useMemo(() => (catalog ?? []).filter((i) => i.kind === "type" && i.repository === repository), [catalog, repository])
  const titles = useMemo(() => new Map(types.map((t) => [t.name, t.title])), [types])
  const name = typeof metadata(draft).name === "string" ? (metadata(draft).name as string) : ""
  const rs = resources(draft)
  const current = rs.find((r) => r.id === selected)
  useBlocker({ shouldBlockFn: () => dirty && !window.confirm("Leave the designer? Your changes are not published."), enableBeforeUnload: () => dirty })

  const publish = useMutation({
    ...publishBlueprintMutation(),
    onSuccess: async (b) => {
      toast.success(`Published ${b.title}`)
      published()
      await queryClient.invalidateQueries({ queryKey: listCatalogQueryKey() })
      await queryClient.invalidateQueries({ queryKey: getBlueprintQueryKey({ path: { repository, name: b.name } }) })
      if (isNew) await navigate({ to: "/designer/$repository/$name", params: { repository, name: b.name } })
    },
  })
  const problems = isProblem(publish.error) ? (publish.error.errors ?? []) : []

  return (
    <div className="grid gap-6">
      <header className="flex flex-wrap items-end justify-between gap-4">
        <div className="grid gap-1">
          <nav aria-label="Breadcrumb" className="text-muted-foreground text-sm">
            <Link to="/designer" className="hover:text-foreground">
              Designer
            </Link>{" "}
            / {repository}
          </nav>
          <h1 className="text-2xl font-semibold tracking-tight">{(metadata(draft).title as string) || (isNew ? "New blueprint" : name)}</h1>
        </div>
        <div className="flex items-center gap-3">
          {dirty && <span className="text-muted-foreground text-sm">Unpublished changes</span>}
          <Button disabled={!name || publish.isPending || (!dirty && !isNew)} onClick={() => publish.mutate({ path: { repository, name }, body: { blueprint: draft } })}>
            <Upload /> Publish
          </Button>
        </div>
      </header>
      {publish.isError && (
        <Alert variant="destructive">
          <AlertCircle />
          <AlertTitle>{problemMessage(publish.error)}</AlertTitle>
          {problems.length > 0 && (
            <AlertDescription>
              <ul className="list-disc pl-4">
                {problems.map((p, i) => (
                  <li key={i}>
                    <code className="font-mono">{(p.location ?? "").replace(/^body\.blueprint\.?/, "") || "blueprint"}</code> {p.message}
                  </li>
                ))}
              </ul>
            </AlertDescription>
          )}
        </Alert>
      )}
      <Tabs defaultValue="resources">
        <TabsList>
          <TabsTrigger value="resources">Resources</TabsTrigger>
          <TabsTrigger value="inputs">Form</TabsTrigger>
          <TabsTrigger value="approval">Approval</TabsTrigger>
          <TabsTrigger value="about">About</TabsTrigger>
          <TabsTrigger value="definition">Definition</TabsTrigger>
        </TabsList>
        <TabsContent value="resources" className="grid gap-4 lg:grid-cols-[14rem_minmax(0,1fr)_20rem]">
          <Card>
            <CardHeader>
              <CardTitle className="text-sm">Types in {repository}</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-1">
              {types.map((t) => (
                <Button
                  key={t.name}
                  type="button"
                  variant="ghost"
                  className="justify-start"
                  aria-label={`Add ${t.title}`}
                  onClick={() => {
                    let id = ""
                    edit((d) => {
                      const r = addResource(d, t.name)
                      id = r.id
                      return r.doc
                    })
                    select(id)
                  }}
                >
                  <Plus /> <span className="truncate">{t.title}</span>
                </Button>
              ))}
              {rs.length > 0 && (
                <nav aria-label="Resources" className="mt-3 grid gap-1 border-t pt-3">
                  {rs.map((r) => (
                    <button
                      key={r.id}
                      type="button"
                      aria-pressed={r.id === selected}
                      className={cn("hover:bg-accent rounded-md px-2 py-1 text-left text-sm", r.id === selected && "bg-accent")}
                      onClick={() => select(r.id)}
                    >
                      {r.id}
                    </button>
                  ))}
                </nav>
              )}
            </CardContent>
          </Card>
          <Canvas titles={titles} />
          <Card>
            <CardHeader>
              <CardTitle className="text-sm">{current ? current.id : "Inspector"}</CardTitle>
            </CardHeader>
            <CardContent>
              {current ? (
                <Inspector key={current.id} repository={repository} resource={current} />
              ) : (
                <p className="text-muted-foreground text-sm">Add a type from the list, or select a resource. Draw an edge from one resource to another to make the second depend on the first.</p>
              )}
            </CardContent>
          </Card>
        </TabsContent>
        <TabsContent value="inputs">
          <InputsEditor />
        </TabsContent>
        <TabsContent value="approval">
          <ApprovalEditor />
        </TabsContent>
        <TabsContent value="about">
          <AboutEditor isNew={isNew} />
        </TabsContent>
        <TabsContent value="definition">
          <pre className="bg-muted/40 overflow-x-auto rounded-md p-3 font-mono text-xs leading-5" aria-label="Definition">
            {JSON.stringify(draft, null, 2)}
          </pre>
        </TabsContent>
      </Tabs>
    </div>
  )
}
