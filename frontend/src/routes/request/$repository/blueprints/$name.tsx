import { useEffect, useMemo, useState } from "react"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { useMutation, useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { AlertCircle } from "lucide-react"
import { toast } from "sonner"
import { getBlueprintOptions, getMeOptions, requestBlueprintMutation } from "@/api/generated/@tanstack/react-query.gen"
import { planBlueprint } from "@/api/generated/sdk.gen"
import type { BlueprintRequestInputBody } from "@/api/generated/types.gen"
import { PlanPreview } from "@/components/plan-preview"
import { ReasonField, RequestHeader, SubmitButton, TextField } from "@/components/request-form"
import { SchemaForm } from "@/components/schema-form/schema-form"
import { defaults, hiddenFields, localErrors, type JsonSchema, type Values } from "@/components/schema-form/schema"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { fieldErrors, hasFieldErrors, problemMessage } from "@/lib/problems"
import { useLivePlan } from "@/lib/use-live-plan"

export const Route = createFileRoute("/request/$repository/blueprints/$name")({
  loader: ({ context, params }) => context.queryClient.ensureQueryData(getBlueprintOptions({ path: params })),
  component: Page,
})

function Page() {
  const { repository, name } = Route.useParams()
  return <BlueprintRequest key={`${repository}/${name}`} repository={repository} name={name} />
}

// An instance name is a file name: what locate.SafeSegment accepts on the server.
const instanceName = /^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$/

function BlueprintRequest({ repository, name }: { repository: string; name: string }) {
  const { data: b } = useSuspenseQuery(getBlueprintOptions({ path: { repository, name } }))
  const { data: me } = useQuery(getMeOptions())
  const navigate = useNavigate()
  const [instance, setInstance] = useState("")
  const [inputs, setInputs] = useState<Values>(() => (defaults(b.schema as JsonSchema, b.schema) ?? {}) as Values)
  const [reason, setReason] = useState("")
  const [touched, setTouched] = useState<Set<string>>(new Set())
  const touch = (path: string) => setTouched((s) => (s.has(path) ? s : new Set(s).add(path)))

  const hidden = useMemo(
    () => hiddenFields(b.ui.fields, inputs, { user: { name: me?.name ?? "", groups: me?.groups ?? [] }, request: { kind: "create" } }),
    [b, inputs, me],
  )
  const body: BlueprintRequestInputBody = { repository, blueprint: name, name: instance, inputs }
  const ready = instance.trim() !== ""
  const { plan, settled } = useLivePlan("plan-blueprint", body, async (b, signal) => (await planBlueprint({ body: b, signal, throwOnError: true })).data, ready)
  const submit = useMutation({
    ...requestBlueprintMutation(),
    onSuccess: async (r) => {
      toast.success(`Request #${r.number} submitted`)
      await navigate({ to: "/requests/$id", params: { id: r.id } })
    },
  })
  const { reset } = submit
  useEffect(() => reset(), [settled, reset]) // a submit error is about the form as it was

  const problem = submit.error ?? plan.error
  const local = localErrors(b.schema as JsonSchema, b.schema, inputs, hidden)
  const inputErrors = { ...Object.fromEntries(Object.entries(local).filter(([p]) => touched.has(p))), ...fieldErrors(problem, "body.inputs") }
  const bodyErrors = fieldErrors(problem, "body")
  const nameError = bodyErrors.name ?? (touched.has("name") && !instanceName.test(instance) ? "use 1 to 63 letters, digits, '.', '_' or '-', starting with a letter or a digit" : undefined)

  return (
    <div className="grid gap-6">
      <RequestHeader title={b.title} description={b.description}>
        <p className="text-muted-foreground text-xs">
          {(b.resources ?? []).length} resource{(b.resources ?? []).length === 1 ? "" : "s"}, requested and approved together
        </p>
      </RequestHeader>
      <div className="grid items-start gap-6 lg:grid-cols-2">
        <form
          className="grid gap-6"
          aria-label={`Request ${b.title}`}
          onSubmit={(e) => {
            e.preventDefault()
            submit.mutate({ body: { ...body, reason } })
          }}
        >
          <Card>
            <CardHeader>
              <CardTitle>Settings</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-5">
              <TextField
                id="instance-name"
                label="Instance name"
                description={`Names this deployment of ${b.title}, to find and change it later.`}
                required
                value={instance}
                error={nameError}
                onChange={setInstance}
                onBlur={() => touch("name")}
              />
              <SchemaForm schema={b.schema} propertyOrder={b.propertyOrder} ui={b.ui} value={inputs} onChange={setInputs} errors={inputErrors} hidden={hidden} onTouch={touch} />
            </CardContent>
          </Card>
          <Card>
            <CardContent className="grid gap-4">
              <ReasonField value={reason} onChange={setReason} needsApproval={(plan.data?.rules?.length ?? 0) > 0} error={bodyErrors.reason} />
              {submit.isError && !hasFieldErrors(submit.error) && (
                <Alert variant="destructive">
                  <AlertCircle />
                  <AlertDescription>{problemMessage(submit.error)}</AlertDescription>
                </Alert>
              )}
              <SubmitButton plan={plan} pending={submit.isPending} />
            </CardContent>
          </Card>
        </form>
        <PlanPreview ready={ready} hint="Name the instance to see the commit." plan={plan} />
      </div>
    </div>
  )
}
