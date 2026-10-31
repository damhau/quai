import { useEffect, useMemo, useState } from "react"
import { useNavigate } from "@tanstack/react-router"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { AlertCircle } from "lucide-react"
import { toast } from "sonner"
import { getMeOptions, getResourceQueryKey, requestTypeMutation } from "@/api/generated/@tanstack/react-query.gen"
import { planType } from "@/api/generated/sdk.gen"
import type { ActionView, ResourceDetail, TypeRequestInputBody, TypeView } from "@/api/generated/types.gen"
import { initialInputs, inputsSchema, inputsUi } from "@/components/day2/inputs-schema"
import { PlanPreview } from "@/components/plan-preview"
import { ReasonField, SubmitButton } from "@/components/request-form"
import { SchemaForm } from "@/components/schema-form/schema-form"
import { hiddenFields, localErrors, setPath, type JsonSchema, type Values } from "@/components/schema-form/schema"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { fieldErrors, hasFieldErrors, problemMessage } from "@/lib/problems"
import { useLivePlan } from "@/lib/use-live-plan"

/**
 * Day2 is a change to an existing resource: edit its settings, run one of its actions, or delete
 * it; or, for a resource its type's migration fixes, migrate every non-conforming resource of the
 * type.
 */
export type Day2 = { kind: "update" } | { kind: "delete" } | { kind: "action"; action: ActionView } | { kind: "migrate" }

export function Day2Dialog({ resource, type, day2, onClose }: { resource: ResourceDetail; type: TypeView; day2: Day2 | null; onClose: () => void }) {
  return (
    <Dialog open={day2 !== null} onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-5xl">
        {day2 && <Day2Form key={day2.kind === "action" ? day2.action.name : day2.kind} resource={resource} type={type} day2={day2} />}
      </DialogContent>
    </Dialog>
  )
}

function title(day2: Day2, name: string) {
  switch (day2.kind) {
    case "update":
      return `Edit ${name}`
    case "delete":
      return `Delete ${name}`
    case "action":
      return `${day2.action.title ?? day2.action.name}: ${name}`
    case "migrate":
      return `Migrate ${name}`
  }
}

function Day2Form({ resource, type, day2 }: { resource: ResourceDetail; type: TypeView; day2: Day2 }) {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { data: me } = useQuery(getMeOptions())
  const doc = useMemo(() => resource.document ?? {}, [resource.document])
  // The form's values as they are: the document itself, or what a mapping type reads back from it.
  const current = useMemo(() => resource.values ?? doc, [resource.values, doc])
  const action = day2.kind === "action" ? day2.action : undefined
  const [values, setValues] = useState<Values>(() => {
    if (action) return initialInputs(action.inputs ?? [], doc)
    let v: Values = structuredClone(current)
    for (const f of type.managedFields ?? []) v = setPath(v, f, undefined)
    return v
  })
  const [reason, setReason] = useState("")
  const [touched, setTouched] = useState<Set<string>>(new Set())
  const touch = (path: string) => setTouched((s) => (s.has(path) ? s : new Set(s).add(path)))

  const schema: unknown = action ? inputsSchema(action.inputs ?? []) : type.schema
  const ui = action ? inputsUi(action.inputs ?? []) : type.ui
  const hidden = useMemo(() => {
    const vars = { params: resource.params, previous: action ? doc : current, user: { name: me?.name ?? "", groups: me?.groups ?? [] }, request: { kind: day2.kind, action: action?.name ?? "" } }
    if (action) return hiddenFields(ui.fields, values, { ...vars, inputs: doc }, "action")
    return new Set([...(type.managedFields ?? []), ...hiddenFields(ui.fields, values, vars)])
  }, [action, ui, values, resource, doc, current, me, day2.kind, type])
  const locked = action ? [] : Object.entries(type.ui.fields ?? {}).flatMap(([p, f]) => (f.immutable ? [p] : []))
  const code = useMemo(() => Object.fromEntries((resource.definedInCode ?? []).map((c) => [c.field, c.text])), [resource.definedInCode])

  const migrate = day2.kind === "migrate"
  const form = day2.kind === "update" || day2.kind === "action"
  const body: TypeRequestInputBody = {
    repository: resource.repository,
    type: resource.type,
    kind: day2.kind,
    ...(!migrate && { key: resource.key }),
    ...(day2.kind === "update" && { values }),
    ...(action && { action: action.name, actionInputs: values }),
  }
  const { plan, settled } = useLivePlan("plan-type", body, async (b, signal) => (await planType({ body: b, signal, throwOnError: true })).data)
  const submit = useMutation({
    ...requestTypeMutation(),
    onSuccess: async (r) => {
      toast.success(`Request #${r.number} submitted`)
      await queryClient.invalidateQueries({ queryKey: getResourceQueryKey({ query: { id: resource.id } }) })
      await navigate({ to: "/requests/$id", params: { id: r.id } })
    },
  })
  const { reset } = submit
  useEffect(() => reset(), [settled, reset]) // a submit error is about the form as it was

  const problem = submit.error ?? plan.error
  const local = form ? localErrors(schema as JsonSchema, schema, values, hidden) : {}
  const errors = { ...Object.fromEntries(Object.entries(local).filter(([p]) => touched.has(p))), ...fieldErrors(problem, action ? "body.actionInputs" : "body.values") }
  const name = migrate ? `every ${type.title}` : (resource.params.name ?? resource.key)
  const unmigrated = plan.data?.unmigrated ?? []

  return (
    <>
      <DialogHeader>
        <DialogTitle>{title(day2, name)}</DialogTitle>
        <DialogDescription>
          {action?.description ??
            (migrate ? (
              <>
                Applies the type&apos;s migration to every {type.title} that does not match its schema, in one change set reviewed like any other.
              </>
            ) : day2.kind === "delete" ? (
              <>
                Removes <code className="font-mono">{resource.path}</code> from the repository; the stack then destroys what it describes.
              </>
            ) : (
              <>
                Changes <code className="font-mono">{resource.path}</code>; comments and the fields you leave alone stay as they are.
              </>
            ))}
        </DialogDescription>
      </DialogHeader>
      <div className="grid items-start gap-6 lg:grid-cols-2">
        <form
          className="grid gap-6"
          aria-label={title(day2, name)}
          onSubmit={(e) => {
            e.preventDefault()
            submit.mutate({ body: { ...body, reason } })
          }}
        >
          {form && (
            <SchemaForm
              schema={schema}
              propertyOrder={action ? ui.order : type.propertyOrder}
              ui={ui}
              value={values}
              onChange={setValues}
              errors={errors}
              hidden={hidden}
              locked={locked}
              code={action ? undefined : code}
              onTouch={touch}
            />
          )}
          {unmigrated.length > 0 && (
            <div className="grid gap-1 text-sm" role="region" aria-label="Left as they are">
              <p className="font-medium">Left as they are, to fix in Git</p>
              <ul className="list-disc pl-4">
                {unmigrated.map((u) => (
                  <li key={u.key}>
                    <code className="font-mono">{u.key}</code>: {u.reason}
                  </li>
                ))}
              </ul>
            </div>
          )}
          <ReasonField value={reason} onChange={setReason} needsApproval={(plan.data?.rules?.length ?? 0) > 0} error={fieldErrors(problem, "body").reason} />
          {submit.isError && !hasFieldErrors(submit.error) && (
            <Alert variant="destructive">
              <AlertCircle />
              <AlertDescription>{problemMessage(submit.error)}</AlertDescription>
            </Alert>
          )}
          <SubmitButton plan={plan} pending={submit.isPending} />
        </form>
        <PlanPreview ready hint="" plan={plan} />
      </div>
    </>
  )
}
