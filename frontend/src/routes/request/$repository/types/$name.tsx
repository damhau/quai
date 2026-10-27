import { useEffect, useMemo, useState } from "react"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { keepPreviousData, useMutation, useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { AlertCircle } from "lucide-react"
import { toast } from "sonner"
import { getMeOptions, getTypeOptions, requestTypeMutation } from "@/api/generated/@tanstack/react-query.gen"
import { planType } from "@/api/generated/sdk.gen"
import type { ParamView, TypeRequestInputBody, TypeView } from "@/api/generated/types.gen"
import { PlanPreview } from "@/components/plan-preview"
import { ReasonField, RequestHeader, SubmitButton, TextField } from "@/components/request-form"
import { SchemaForm, EnumControl } from "@/components/schema-form/schema-form"
import { defaults, hiddenFields, localErrors, setPath, type JsonSchema, type Values } from "@/components/schema-form/schema"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Label } from "@/components/ui/label"
import { fieldErrors, hasFieldErrors, problemMessage } from "@/lib/problems"
import { useDebounced } from "@/lib/use-debounced"

export const Route = createFileRoute("/request/$repository/types/$name")({
  loader: ({ context, params }) => context.queryClient.ensureQueryData(getTypeOptions({ path: params })),
  component: Page,
})

function Page() {
  const { repository, name } = Route.useParams()
  return <TypeRequest key={`${repository}/${name}`} repository={repository} name={name} />
}

/** initialValues are the schema's defaults, without the fields Quai sets itself. */
function initialValues(t: TypeView): Values {
  let v = (defaults(t.schema as JsonSchema, t.schema) ?? {}) as Values
  for (const f of t.managedFields ?? []) v = setPath(v, f, undefined)
  return v
}

/** paramError checks a locator param as the server does: present, matching its pattern and options. */
function paramError(p: ParamView, v: string): string | undefined {
  if (v.trim() === "") return "is required"
  if (p.pattern) {
    try {
      if (!new RegExp(`^(?:${p.pattern})$`, "u").test(v)) return `must match ${p.pattern}`
    } catch {
      return undefined
    }
  }
  if (p.options?.length && !p.options.includes(v)) return `must be one of ${p.options.join(", ")}`
  return undefined
}

function TypeRequest({ repository, name }: { repository: string; name: string }) {
  const { data: t } = useSuspenseQuery(getTypeOptions({ path: { repository, name } }))
  const { data: me } = useQuery(getMeOptions())
  const navigate = useNavigate()
  const typeParams = t.params ?? []
  const [params, setParams] = useState<Record<string, string>>(() => Object.fromEntries(typeParams.map((p) => [p.name, p.options?.length === 1 ? p.options[0]! : ""])))
  const [values, setValues] = useState<Values>(() => initialValues(t))
  const [reason, setReason] = useState("")
  const [touched, setTouched] = useState<Set<string>>(new Set())
  const touch = (path: string) => setTouched((s) => (s.has(path) ? s : new Set(s).add(path)))

  const hidden = useMemo(() => {
    const vars = { params, user: { name: me?.name ?? "", groups: me?.groups ?? [] }, request: { kind: "create" } }
    return new Set([...(t.managedFields ?? []), ...hiddenFields(t.ui.fields, values, vars)])
  }, [t, values, params, me])

  const body: TypeRequestInputBody = { repository, type: name, kind: "create", params, values }
  const ready = typeParams.every((p) => params[p.name]?.trim())
  const settled = useDebounced(JSON.stringify(body), 300)
  const plan = useQuery({
    queryKey: ["plan-type", settled],
    queryFn: async ({ signal }) => (await planType({ body: JSON.parse(settled) as TypeRequestInputBody, signal, throwOnError: true })).data,
    enabled: ready,
    placeholderData: keepPreviousData,
  })
  const submit = useMutation({
    ...requestTypeMutation(),
    onSuccess: async (r) => {
      toast.success(`Request #${r.number} submitted`)
      await navigate({ to: "/requests/$id", params: { id: r.id } })
    },
  })
  const { reset } = submit
  useEffect(() => reset(), [settled, reset]) // a submit error is about the form as it was

  const problem = submit.error ?? plan.error
  const local = localErrors(t.schema as JsonSchema, t.schema, values, hidden)
  const valueErrors = { ...Object.fromEntries(Object.entries(local).filter(([p]) => touched.has(p))), ...fieldErrors(problem, "body.values") }
  const paramErrors = fieldErrors(problem, "body.params")

  return (
    <div className="grid gap-6">
      <RequestHeader title={t.title} description={t.description}>
        <p className="text-muted-foreground text-xs">
          Written to <code className="font-mono">{t.locate}</code>
          {t.stack && (
            <>
              {" "}
              · applied by the stack <code className="font-mono">{t.stack}</code>
            </>
          )}
        </p>
      </RequestHeader>
      <div className="grid items-start gap-6 lg:grid-cols-2">
        <form
          className="grid gap-6"
          aria-label={`Request a ${t.title}`}
          onSubmit={(e) => {
            e.preventDefault()
            submit.mutate({ body: { ...body, reason } })
          }}
        >
          {typeParams.length > 0 && (
            <Card>
              <CardHeader>
                <CardTitle>Where</CardTitle>
                <CardDescription>Which file the resource is written to.</CardDescription>
              </CardHeader>
              <CardContent className="grid gap-5">
                {typeParams.map((p) => {
                  const id = `param-${p.name}`
                  const error = paramErrors[p.name] ?? (touched.has(`params.${p.name}`) ? paramError(p, params[p.name] ?? "") : undefined)
                  const set = (v: string) => setParams((ps) => ({ ...ps, [p.name]: v }))
                  if (p.options?.length)
                    return (
                      <div key={p.name} className="grid gap-2">
                        <Label htmlFor={id}>
                          {p.title ?? p.name}
                          <span className="text-destructive" aria-hidden>
                            *
                          </span>
                        </Label>
                        <EnumControl id={id} title={p.title ?? p.name} required disabled={false} options={p.options} value={params[p.name] || undefined} onChange={(v) => set(String(v ?? ""))} />
                        {p.description && <p className="text-muted-foreground text-xs">{p.description}</p>}
                        {error && <p className="text-destructive text-xs">{error}</p>}
                      </div>
                    )
                  return (
                    <TextField
                      key={p.name}
                      id={id}
                      label={p.title ?? p.name}
                      description={p.description}
                      required
                      value={params[p.name] ?? ""}
                      error={error}
                      onChange={set}
                      onBlur={() => touch(`params.${p.name}`)}
                    />
                  )
                })}
              </CardContent>
            </Card>
          )}
          <Card>
            <CardHeader>
              <CardTitle>Settings</CardTitle>
            </CardHeader>
            <CardContent>
              <SchemaForm schema={t.schema} propertyOrder={t.propertyOrder} ui={t.ui} value={values} onChange={setValues} errors={valueErrors} hidden={hidden} onTouch={touch} />
            </CardContent>
          </Card>
          <Card>
            <CardContent className="grid gap-4">
              <ReasonField value={reason} onChange={setReason} needsApproval={(plan.data?.rules?.length ?? 0) > 0} error={fieldErrors(problem, "body").reason} />
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
        <PlanPreview ready={ready} hint={`Fill in the ${typeParams.map((p) => p.title ?? p.name).join(" and ")} to see the commit.`} plan={plan} />
      </div>
    </div>
  )
}
