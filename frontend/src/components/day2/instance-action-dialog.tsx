import { useEffect, useMemo, useState } from "react"
import { useNavigate } from "@tanstack/react-router"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { AlertCircle } from "lucide-react"
import { toast } from "sonner"
import { getInstanceQueryKey, getMeOptions, requestInstanceActionMutation } from "@/api/generated/@tanstack/react-query.gen"
import { planInstanceAction } from "@/api/generated/sdk.gen"
import type { ActionView, InstanceActionInput, InstanceView } from "@/api/generated/types.gen"
import { initialInputs, inputsSchema, inputsUi } from "@/components/day2/inputs-schema"
import { PlanPreview } from "@/components/plan-preview"
import { ReasonField, SubmitButton } from "@/components/request-form"
import { SchemaForm } from "@/components/schema-form/schema-form"
import { hiddenFields, localErrors, type Values } from "@/components/schema-form/schema"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { fieldErrors, hasFieldErrors, problemMessage } from "@/lib/problems"
import { useLivePlan } from "@/lib/use-live-plan"

/** InstanceActionDialog runs one of a blueprint's day-2 actions on an instance. */
export function InstanceActionDialog({ instance, action, onClose }: { instance: InstanceView; action: ActionView | null; onClose: () => void }) {
  return (
    <Dialog open={action !== null} onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-5xl">{action && <ActionForm key={action.name} instance={instance} action={action} />}</DialogContent>
    </Dialog>
  )
}

function ActionForm({ instance, action }: { instance: InstanceView; action: ActionView }) {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { data: me } = useQuery(getMeOptions())
  const inputs = useMemo(() => action.inputs ?? [], [action])
  const [values, setValues] = useState<Values>(() => initialInputs(inputs, instance.inputs))
  const [reason, setReason] = useState("")
  const [touched, setTouched] = useState<Set<string>>(new Set())
  const touch = (path: string) => setTouched((s) => (s.has(path) ? s : new Set(s).add(path)))
  const schema = useMemo(() => inputsSchema(inputs), [inputs])
  const ui = useMemo(() => inputsUi(inputs), [inputs])
  const hidden = useMemo(
    () => hiddenFields(ui.fields, values, { inputs: instance.inputs, user: { name: me?.name ?? "", groups: me?.groups ?? [] }, request: { kind: "action", action: action.name } }, "action"),
    [ui, values, instance, me, action],
  )

  const body: InstanceActionInput = { repository: instance.repository, blueprint: instance.blueprint, instance: instance.name, action: action.name, actionInputs: values }
  const { plan, settled } = useLivePlan("plan-instance-action", body, async (b, signal) => (await planInstanceAction({ body: b, signal, throwOnError: true })).data)
  const submit = useMutation({
    ...requestInstanceActionMutation(),
    onSuccess: async (r) => {
      toast.success(`Request #${r.number} submitted`)
      await queryClient.invalidateQueries({ queryKey: getInstanceQueryKey({ path: { repository: instance.repository, blueprint: instance.blueprint, name: instance.name } }) })
      await navigate({ to: "/requests/$id", params: { id: r.id } })
    },
  })
  const { reset } = submit
  useEffect(() => reset(), [settled, reset]) // a submit error is about the form as it was

  const problem = submit.error ?? plan.error
  const local = localErrors(schema, schema, values, hidden)
  const errors = { ...Object.fromEntries(Object.entries(local).filter(([p]) => touched.has(p))), ...fieldErrors(problem, "body.actionInputs") }
  const title = `${action.title ?? action.name}: ${instance.name}`
  return (
    <>
      <DialogHeader>
        <DialogTitle>{title}</DialogTitle>
        <DialogDescription>{action.description ?? `Changes the resources of ${instance.name} that this action applies to.`}</DialogDescription>
      </DialogHeader>
      <div className="grid items-start gap-6 lg:grid-cols-2">
        <form
          className="grid gap-6"
          aria-label={title}
          onSubmit={(e) => {
            e.preventDefault()
            submit.mutate({ body: { ...body, reason } })
          }}
        >
          {inputs.length > 0 && <SchemaForm schema={schema} propertyOrder={ui.order} ui={ui} value={values} onChange={setValues} errors={errors} hidden={hidden} onTouch={touch} />}
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
