import { useState } from "react"
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Loader2, Save, Unplug } from "lucide-react"
import { toast } from "sonner"
import {
  deleteRepositoryMutation,
  getRepositoryOptions,
  getRepositoryQueryKey,
  listRepositoryConnectionsQueryKey,
  repositoryHealthQueryKey,
  updateRepositoryMutation,
} from "@/api/generated/@tanstack/react-query.gen"
import type { ConnectionView, Document } from "@/api/generated/types.gen"
import { RunToken } from "@/components/repositories/checks"
import { Problems } from "@/components/repositories/problems"
import { ConnectionFields, DeliveryFields, RunnerFields, WebhookFields } from "@/components/repositories/repository-form"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog"
import { Skeleton } from "@/components/ui/skeleton"
import { problemMessage } from "@/lib/problems"
import { pruned } from "@/lib/repositories"

export const Route = createFileRoute("/admin/repositories/$name/edit")({
  component: Edit,
})

function Edit() {
  const { name } = Route.useParams()
  const { data, isPending, error } = useQuery(getRepositoryOptions({ path: { repository: name } }))
  return (
    <section aria-labelledby="edit-title" className="grid gap-6">
      <header className="grid gap-1">
        <nav aria-label="Breadcrumb" className="text-muted-foreground text-sm">
          <Link to="/admin/repositories" className="hover:text-foreground">
            Repositories
          </Link>{" "}
          /{" "}
          <Link to="/admin/repositories/$name" params={{ name }} className="hover:text-foreground">
            {name}
          </Link>{" "}
          / Edit
        </nav>
        <h1 id="edit-title" className="text-2xl font-semibold tracking-tight">
          Edit {data?.document.metadata.title || name}
        </h1>
      </header>
      {isPending && <Skeleton className="h-64" />}
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{problemMessage(error)}</AlertDescription>
        </Alert>
      )}
      {data && <EditForm key={name} connection={data} />}
    </section>
  )
}

function EditForm({ connection }: { connection: ConnectionView }) {
  const name = connection.name
  const [doc, setDoc] = useState<Document>(connection.document)
  const readOnly = connection.managedBy === "config"
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const refresh = () =>
    Promise.all([
      queryClient.invalidateQueries({
        queryKey: getRepositoryQueryKey({ path: { repository: name } }),
      }),
      queryClient.invalidateQueries({
        queryKey: repositoryHealthQueryKey({ path: { repository: name } }),
      }),
      queryClient.invalidateQueries({
        queryKey: listRepositoryConnectionsQueryKey(),
      }),
    ])
  const save = useMutation({
    ...updateRepositoryMutation(),
    onSuccess: async () => {
      await refresh()
      toast.success(`${name} saved`)
      await navigate({ to: "/admin/repositories/$name", params: { name } })
    },
  })
  const remove = useMutation({
    ...deleteRepositoryMutation(),
    onSuccess: async () => {
      await queryClient.invalidateQueries({
        queryKey: listRepositoryConnectionsQueryKey(),
      })
      toast.success(`${name} disconnected`)
      await navigate({ to: "/admin/repositories" })
    },
  })
  return (
    <form
      className="grid gap-6"
      onSubmit={(e) => {
        e.preventDefault()
        save.mutate({ path: { repository: name }, body: pruned(doc) })
      }}
    >
      {readOnly && (
        <Alert role="status">
          <AlertTitle>Declared in Quai's configuration</AlertTitle>
          <AlertDescription>Change it in the configuration (repositories.path or the Helm chart's repositories value); here it is read-only.</AlertDescription>
        </Alert>
      )}
      <Card>
        <CardHeader>
          <CardTitle>Connection</CardTitle>
        </CardHeader>
        <CardContent>
          <ConnectionFields doc={doc} onChange={setDoc} disabled={readOnly} editing />
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Delivery</CardTitle>
        </CardHeader>
        <CardContent>
          <DeliveryFields doc={doc} onChange={setDoc} disabled={readOnly} />
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Runner and webhook</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-4">
          <RunnerFields doc={doc} onChange={setDoc} disabled={readOnly} />
          {!readOnly && <RunToken repository={name} />}
          <WebhookFields doc={doc} onChange={setDoc} disabled={readOnly} />
        </CardContent>
      </Card>
      {save.isError && <Problems error={save.error} />}
      {remove.isError && <Problems error={remove.error} />}
      {!readOnly && (
        <div className="flex flex-wrap gap-2">
          <Button type="submit" disabled={save.isPending}>
            {save.isPending ? <Loader2 className="animate-spin" /> : <Save />} Save
          </Button>
          <Dialog>
            <DialogTrigger asChild>
              <Button type="button" variant="outline">
                <Unplug /> Disconnect
              </Button>
            </DialogTrigger>
            <DialogContent>
              <DialogHeader>
                <DialogTitle>Disconnect {name}?</DialogTitle>
                <DialogDescription>Quai stops working with the repository. Its requests and their history stay; nothing changes in Git.</DialogDescription>
              </DialogHeader>
              <DialogFooter>
                <DialogClose asChild>
                  <Button type="button" variant="outline">
                    Keep it
                  </Button>
                </DialogClose>
                <Button type="button" variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate({ path: { repository: name } })}>
                  {remove.isPending ? <Loader2 className="animate-spin" /> : <Unplug />} Disconnect
                </Button>
              </DialogFooter>
            </DialogContent>
          </Dialog>
        </div>
      )}
    </form>
  )
}
