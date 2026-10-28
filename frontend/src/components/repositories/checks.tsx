import { useMutation } from "@tanstack/react-query"
import { CheckCircle2, CircleSlash, Copy, KeyRound, Loader2, ShieldAlert, ShieldCheck, Webhook, XCircle } from "lucide-react"
import { toast } from "sonner"
import { checkRepositoryEnforcementMutation, generateRunTokenMutation, installRepositoryWebhookMutation } from "@/api/generated/@tanstack/react-query.gen"
import type { AccessView, Document, EnforcementView } from "@/api/generated/types.gen"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { problemMessage } from "@/lib/problems"
import { checkTitles, protectionGuide, pruned } from "@/lib/repositories"

/** AccessReport shows each check of the access test, with the permission a failed one needs, then
 * what the scan found. */
export function AccessReport({ report }: { report: AccessView }) {
  return (
    <div className="grid gap-4">
      <ol aria-label="Access checks" className="grid gap-2">
        {(report.checks ?? []).map((c) => (
          <li key={c.name} className="flex items-start gap-2 text-sm">
            {c.ok ? (
              <CheckCircle2 className="mt-0.5 size-4 shrink-0 text-emerald-600" aria-label="Passed" />
            ) : c.skipped ? (
              <CircleSlash className="text-muted-foreground mt-0.5 size-4 shrink-0" aria-label="Skipped" />
            ) : (
              <XCircle className="text-destructive mt-0.5 size-4 shrink-0" aria-label="Failed" />
            )}
            <div className="grid gap-0.5">
              <span className="font-medium">{checkTitles[c.name] ?? c.name}</span>
              {c.skipped && <span className="text-muted-foreground">Not run: {c.skipped}</span>}
              {c.error && <span className="text-destructive break-words">{c.error}</span>}
              {c.permission && <span>Needs: {c.permission}</span>}
            </div>
          </li>
        ))}
      </ol>
      {report.scan &&
        (report.scan.found ? (
          <p className="text-sm">
            Found <code>.quai/</code> on the branch: {report.scan.types} types and {report.scan.blueprints} blueprints
            {(report.scan.problems?.length ?? 0) > 0 && <>, with {report.scan.problems?.length} problems to fix in Git</>}.
          </p>
        ) : (
          <p className="text-sm">
            No <code>.quai/</code> on the branch yet: the onboarding assistant will propose types from the repository's files. Until then, the catalog shows nothing from it.
          </p>
        ))}
    </div>
  )
}

/** Enforcement shows the branch protection Quai needs, verifies it on the platform and says
 * what is missing. */
export function Enforcement({ doc }: { doc: Document }) {
  const verify = useMutation(checkRepositoryEnforcementMutation())
  const branch = doc.spec.defaultBranch || "main"
  const check = doc.spec.enforcement?.requiredCheck || "quai/approval"
  const steps = protectionGuide(doc.spec.connector, branch, check)
  return (
    <div className="grid gap-3">
      <ol className="list-decimal space-y-1 pl-5 text-sm" aria-label="Branch protection to set">
        {steps.map((s) => (
          <li key={s}>{s}</li>
        ))}
      </ol>
      {doc.spec.connector !== "generic" && (
        <Button type="button" variant="outline" className="w-fit" disabled={verify.isPending} onClick={() => verify.mutate({ body: pruned(doc) })}>
          {verify.isPending ? <Loader2 className="animate-spin" /> : <ShieldCheck />} Verify the protection
        </Button>
      )}
      {verify.isError && (
        <Alert variant="destructive">
          <AlertDescription>{problemMessage(verify.error)}</AlertDescription>
        </Alert>
      )}
      {verify.data && <EnforcementResult enforcement={verify.data} branch={branch} check={check} />}
    </div>
  )
}

export function EnforcementResult({ enforcement, branch, check }: { enforcement: EnforcementView; branch: string; check: string }) {
  if (!enforcement.required) return <p className="text-muted-foreground text-sm">Direct delivery opens no pull requests: there is nothing to enforce.</p>
  if (!enforcement.checked) return <p className="text-muted-foreground text-sm">Not verified yet.</p>
  if ((enforcement.problems?.length ?? 0) === 0)
    return (
      <Alert role="status">
        <ShieldCheck />
        <AlertTitle>Approvals are enforced</AlertTitle>
        <AlertDescription>
          {branch} requires {check} as Quai needs it.
        </AlertDescription>
      </Alert>
    )
  return (
    <Alert variant="destructive" role="alert">
      <ShieldAlert />
      <AlertTitle>Approvals are not enforced: Quai opens no pull request</AlertTitle>
      <AlertDescription>
        <ul className="list-disc pl-4">
          {enforcement.problems?.map((p) => (
            <li key={p}>{p}</li>
          ))}
        </ul>
      </AlertDescription>
    </Alert>
  )
}

async function copy(text: string, what: string) {
  try {
    await navigator.clipboard.writeText(text)
    toast.success(`${what} copied`)
  } catch {
    toast.error("The browser did not allow copying: select the text instead")
  }
}

/** RunToken generates a run token and shows it once, with its hash. */
export function RunToken({ repository }: { repository: string }) {
  const generate = useMutation(generateRunTokenMutation())
  return (
    <div className="grid gap-3">
      <Button type="button" variant="outline" className="w-fit" disabled={!repository || generate.isPending} onClick={() => generate.mutate({ path: { repository } })}>
        {generate.isPending ? <Loader2 className="animate-spin" /> : <KeyRound />} Generate a run token
      </Button>
      {generate.isError && <p className="text-destructive text-sm">{problemMessage(generate.error)}</p>}
      {generate.data && (
        <div className="grid gap-3 text-sm" aria-label="Run token">
          <p>Shown once: Quai keeps neither of these.</p>
          <TokenLine label="For the pipeline (a bearer token)" value={generate.data.token} />
          <TokenLine label="For Quai: store this SHA-256 in the secret the run token reference names" value={generate.data.sha256} />
        </div>
      )}
    </div>
  )
}

function TokenLine({ label, value }: { label: string; value: string }) {
  return (
    <div className="grid gap-1">
      <span className="text-muted-foreground">{label}</span>
      <div className="flex items-center gap-2">
        <code className="bg-muted rounded px-2 py-1 text-xs break-all">{value}</code>
        <Button type="button" size="icon" variant="ghost" aria-label={`Copy: ${label}`} onClick={() => void copy(value, "Copied value")}>
          <Copy />
        </Button>
      </div>
    </div>
  )
}

/** InstallWebhook installs the repository's webhook through the platform, or says how to set it
 * up by hand. */
export function InstallWebhook({ repository }: { repository: string }) {
  const install = useMutation(installRepositoryWebhookMutation())
  return (
    <div className="grid gap-3">
      <Button type="button" variant="outline" className="w-fit" disabled={install.isPending} onClick={() => install.mutate({ path: { repository } })}>
        {install.isPending ? <Loader2 className="animate-spin" /> : <Webhook />} Install the webhook
      </Button>
      {install.isError && <p className="text-destructive text-sm">{problemMessage(install.error)}</p>}
      {install.data &&
        (install.data.installed ? (
          <p className="text-sm" role="status">
            Installed: the platform delivers events to <code>{install.data.url}</code>.
          </p>
        ) : (
          <p className="text-sm" role="status">
            {install.data.manual}
          </p>
        ))}
    </div>
  )
}
