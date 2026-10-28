import { useEffect, useState } from "react"
import { createFileRoute, Link } from "@tanstack/react-router"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { ArrowLeft, ArrowRight, Loader2, PlugZap, ScanSearch } from "lucide-react"
import { createRepositoryMutation, listRepositoryConnectionsQueryKey, testRepositoryAccessMutation } from "@/api/generated/@tanstack/react-query.gen"
import type { Document } from "@/api/generated/types.gen"
import { AccessReport, Enforcement, InstallWebhook, RunToken } from "@/components/repositories/checks"
import { ConnectionFields, DeliveryFields, RunnerFields, WebhookFields } from "@/components/repositories/repository-form"
import { Problems } from "@/components/repositories/problems"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button, buttonVariants } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { newDocument, platformLabel, pruned } from "@/lib/repositories"
import { cn } from "@/lib/utils"

export const Route = createFileRoute("/admin/repositories/new")({
  component: Wizard,
})

const steps = [
  {
    title: "Platform and URL",
    description: "Where the repository is, and the credential Quai uses with it.",
  },
  {
    title: "Access test",
    description: "Quai checks that the credential can do what it does, and looks for definitions.",
  },
  {
    title: "Delivery and enforcement",
    description: "How changes reach the repository, and the branch protection that enforces Quai's approval.",
  },
  { title: "Runner", description: "Where plan and apply results come from." },
  {
    title: "Webhook",
    description: "How the platform tells Quai about pushes and pull requests.",
  },
  { title: "Connect", description: "Save the connection." },
] as const

function Wizard() {
  const [doc, setDoc] = useState<Document>(newDocument)
  const [step, setStep] = useState(0)
  const queryClient = useQueryClient()
  const access = useMutation(testRepositoryAccessMutation())
  const create = useMutation({
    ...createRepositoryMutation(),
    onSuccess: () =>
      queryClient.invalidateQueries({
        queryKey: listRepositoryConnectionsQueryKey(),
      }),
  })
  const spec = doc.spec
  // A report is about the connection as it was tested.
  const tested = JSON.stringify([doc.metadata.name, spec.connector, spec.url, spec.defaultBranch, spec.credentials, spec.delivery?.branchPrefix])
  const { reset } = access
  useEffect(() => reset(), [tested, reset])

  const canNext = [doc.metadata.name !== "" && spec.url !== "", access.data?.ok === true, true, true, true][step] ?? false
  const current = steps[step] ?? steps[0]
  return (
    <section aria-labelledby="wizard-title" className="grid gap-6">
      <header className="grid gap-1">
        <nav aria-label="Breadcrumb" className="text-muted-foreground text-sm">
          <Link to="/admin/repositories" className="hover:text-foreground">
            Repositories
          </Link>{" "}
          / Connect
        </nav>
        <h1 id="wizard-title" className="text-2xl font-semibold tracking-tight">
          Connect a repository
        </h1>
      </header>
      <ol aria-label="Steps" className="flex flex-wrap gap-2 text-sm">
        {steps.map((s, i) => (
          <li
            key={s.title}
            aria-current={i === step ? "step" : undefined}
            className={cn("rounded-md px-2.5 py-1", i === step ? "bg-accent text-foreground" : "text-muted-foreground")}
          >
            {i + 1}. {s.title}
          </li>
        ))}
      </ol>
      <Card>
        <CardHeader>
          <CardTitle>{current.title}</CardTitle>
          <CardDescription>{current.description}</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          {step === 0 && <ConnectionFields doc={doc} onChange={setDoc} />}
          {step === 1 && (
            <>
              <Button type="button" className="w-fit" disabled={access.isPending} onClick={() => access.mutate({ body: pruned(doc) })}>
                {access.isPending ? <Loader2 className="animate-spin" /> : <ScanSearch />} Test access
              </Button>
              {access.isError && <Problems error={access.error} />}
              {access.data && <AccessReport report={access.data} />}
              {access.data && !access.data.ok && <p className="text-sm">Give the credential what it needs, or change the connection, then test again.</p>}
            </>
          )}
          {step === 2 && (
            <>
              <DeliveryFields doc={doc} onChange={setDoc} />
              {(spec.delivery?.mode ?? "pull-request") === "pull-request" && spec.connector !== "generic" && <Enforcement doc={doc} />}
            </>
          )}
          {step === 3 && (
            <>
              <RunnerFields doc={doc} onChange={setDoc} />
              <RunToken repository={doc.metadata.name} />
            </>
          )}
          {step === 4 && (
            <>
              <WebhookFields doc={doc} onChange={setDoc} />
              <p className="text-muted-foreground text-sm">Once connected, Quai installs the webhook through the platform where the credential allows it.</p>
            </>
          )}
          {step === 5 && !create.data && (
            <>
              <dl className="grid gap-x-6 gap-y-1 text-sm sm:grid-cols-[max-content_1fr]">
                <dt className="text-muted-foreground">Name</dt>
                <dd>{doc.metadata.name}</dd>
                <dt className="text-muted-foreground">Platform</dt>
                <dd>{platformLabel(spec.connector)}</dd>
                <dt className="text-muted-foreground">URL</dt>
                <dd className="break-all">{spec.url}</dd>
                <dt className="text-muted-foreground">Delivery</dt>
                <dd>{spec.delivery?.mode === "direct" ? "Direct commits" : `Pull requests, merged ${spec.delivery?.merge === "manual" ? "by a person" : "by Quai"}`}</dd>
              </dl>
              {create.isError && <Problems error={create.error} />}
              <Button type="button" className="w-fit" disabled={create.isPending} onClick={() => create.mutate({ body: pruned(doc) })}>
                {create.isPending ? <Loader2 className="animate-spin" /> : <PlugZap />} Connect
              </Button>
            </>
          )}
          {step === 5 && create.data && (
            <>
              {create.data.connected ? (
                <Alert role="status">
                  <PlugZap />
                  <AlertTitle>Connected</AlertTitle>
                  <AlertDescription>{create.data.document.metadata.title || create.data.name} is connected.</AlertDescription>
                </Alert>
              ) : (
                <Alert variant="destructive" role="alert">
                  <AlertTitle>Saved, not connected yet</AlertTitle>
                  <AlertDescription>{create.data.error ?? "Quai retries in a few seconds."}</AlertDescription>
                </Alert>
              )}
              {spec.webhooks?.secret?.ref && <InstallWebhook repository={create.data.name} />}
              <Link to="/admin/repositories/$name" params={{ name: create.data.name }} className={cn(buttonVariants({ variant: "outline" }), "w-fit")}>
                Open its health
              </Link>
            </>
          )}
        </CardContent>
      </Card>
      {!create.data && (
        <div className="flex gap-2">
          <Button type="button" variant="outline" disabled={step === 0} onClick={() => setStep(step - 1)}>
            <ArrowLeft /> Back
          </Button>
          {step < steps.length - 1 && (
            <Button type="button" disabled={!canNext} onClick={() => setStep(step + 1)}>
              Next <ArrowRight />
            </Button>
          )}
        </div>
      )}
    </section>
  )
}
