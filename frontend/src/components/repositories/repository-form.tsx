import type { Document } from "@/api/generated/types.gen"
import { TextField } from "@/components/request-form"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Textarea } from "@/components/ui/textarea"
import { platforms, set } from "@/lib/repositories"

// The fields of a Repository document, in the sections the wizard and the edit page show.

export type FieldsProps = {
  doc: Document
  onChange: (d: Document) => void
  disabled?: boolean
  editing?: boolean
}

function Select({
  id,
  label,
  value,
  options,
  onChange,
  disabled,
  description,
}: {
  id: string
  label: string
  value: string
  options: readonly { value: string; label: string }[]
  onChange: (v: string) => void
  disabled?: boolean
  description?: string
}) {
  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{label}</Label>
      <NativeSelect id={id} value={value} disabled={disabled} onChange={(e) => onChange(e.target.value)}>
        {options.map((o) => (
          <NativeSelectOption key={o.value} value={o.value}>
            {o.label}
          </NativeSelectOption>
        ))}
      </NativeSelect>
      {description && <p className="text-muted-foreground text-xs">{description}</p>}
    </div>
  )
}

/** ConnectionFields: the platform, where the repository is, and the credential Quai uses. */
export function ConnectionFields({ doc, onChange, disabled, editing }: FieldsProps) {
  const spec = doc.spec
  const platform = platforms.find((p) => p.value === spec.connector)
  const change = (path: string) => (v: unknown) => onChange(set(doc, path, v))
  return (
    <div className="grid gap-4 sm:grid-cols-2">
      <TextField
        id="name"
        label="Name"
        required
        value={doc.metadata.name}
        disabled={disabled || editing}
        onChange={change("metadata.name")}
        description="Part of every resource ID; it cannot change later. Lowercase letters, digits and dashes."
      />
      <TextField id="title" label="Title" value={doc.metadata.title ?? ""} disabled={disabled} onChange={change("metadata.title")} />
      <Select
        id="connector"
        label="Platform"
        value={spec.connector}
        options={platforms}
        disabled={disabled}
        onChange={(v) => {
          let next = set(doc, "spec.connector", v)
          if (v === "generic") next = set(next, "spec.delivery.mode", "direct")
          onChange(next)
        }}
      />
      <TextField id="url" label="Repository URL" required value={spec.url} disabled={disabled} placeholder={platform?.url} onChange={change("spec.url")} />
      <TextField id="defaultBranch" label="Target branch" value={spec.defaultBranch ?? ""} disabled={disabled} placeholder="main" onChange={change("spec.defaultBranch")} />
      <TextField
        id="credentials"
        label="Credential reference"
        value={spec.credentials?.ref ?? ""}
        disabled={disabled}
        placeholder={spec.connector === "generic" ? "k8s:quai/deploy-key#ssh-privatekey" : "k8s:quai/infra-live-pat"}
        onChange={change("spec.credentials.ref")}
        description="Where the secret is (k8s:<namespace>/<name>#<key> or env:<NAME>), never the secret itself."
      />
      {spec.connector === "generic" && (
        <div className="grid gap-2 sm:col-span-2">
          <Label htmlFor="knownHosts">Host keys</Label>
          <Textarea
            id="knownHosts"
            rows={3}
            value={spec.credentials?.knownHosts ?? ""}
            disabled={disabled}
            className="font-mono text-xs"
            placeholder="git.example.com ssh-ed25519 AAAA…"
            onChange={(e) => onChange(set(doc, "spec.credentials.knownHosts", e.target.value))}
          />
          <p className="text-muted-foreground text-xs">The server's known_hosts lines: Quai always verifies the host key.</p>
        </div>
      )}
    </div>
  )
}

/** DeliveryFields: how changes reach the repository. */
export function DeliveryFields({ doc, onChange, disabled }: FieldsProps) {
  const d = doc.spec.delivery ?? {}
  const generic = doc.spec.connector === "generic"
  const change = (path: string) => (v: unknown) => onChange(set(doc, path, v))
  return (
    <div className="grid gap-4 sm:grid-cols-2">
      <Select
        id="mode"
        label="Delivery"
        value={d.mode || (generic ? "direct" : "pull-request")}
        disabled={disabled || generic}
        options={[
          { value: "pull-request", label: "Pull requests" },
          { value: "direct", label: "Direct commits (sandboxes and demos)" },
        ]}
        onChange={change("spec.delivery.mode")}
        description={generic ? "Generic Git has no pull requests." : undefined}
      />
      <TextField id="branchPrefix" label="Branch prefix" value={d.branchPrefix ?? ""} disabled={disabled} placeholder="quai/" onChange={change("spec.delivery.branchPrefix")} />
      {(d.mode || (generic ? "direct" : "pull-request")) === "pull-request" && (
        <>
          <Select
            id="merge"
            label="Merge"
            value={d.merge || "auto"}
            disabled={disabled}
            options={[
              { value: "auto", label: "Quai merges once approved and green" },
              { value: "manual", label: "A person merges" },
            ]}
            onChange={change("spec.delivery.merge")}
          />
          <Select
            id="mergeMethod"
            label="Merge method"
            value={d.mergeMethod || "squash"}
            disabled={disabled}
            options={[
              { value: "squash", label: "Squash" },
              { value: "merge", label: "Merge commit" },
              { value: "rebase", label: "Rebase" },
            ]}
            onChange={change("spec.delivery.mergeMethod")}
          />
        </>
      )}
    </div>
  )
}

/** RunnerFields: where plan and apply results come from. */
export function RunnerFields({ doc, onChange, disabled }: FieldsProps) {
  const r = doc.spec.runner ?? {}
  return (
    <div className="grid gap-4 sm:grid-cols-2">
      <Select
        id="runner"
        label="Runner"
        value={r.kind ?? ""}
        disabled={disabled}
        options={[
          { value: "", label: "None: resources show In repo after merge" },
          { value: "azure-pipelines", label: "Azure Pipelines" },
        ]}
        onChange={(v) => onChange(set(doc, "spec.runner.kind", v))}
      />
      <TextField
        id="reportToken"
        label="Run token reference"
        value={r.reportToken?.ref ?? ""}
        disabled={disabled}
        placeholder="k8s:quai/infra-live-run-token"
        onChange={(v) => onChange(set(doc, "spec.runner.reportToken.ref", v))}
        description="The secret holding the run token's SHA-256."
      />
    </div>
  )
}

/** WebhookFields: the secret the platform's webhooks carry. */
export function WebhookFields({ doc, onChange, disabled }: FieldsProps) {
  return (
    <TextField
      id="webhookSecret"
      label="Webhook secret reference"
      value={doc.spec.webhooks?.secret?.ref ?? ""}
      disabled={disabled}
      placeholder="k8s:quai/infra-live-webhook-secret"
      onChange={(v) => onChange(set(doc, "spec.webhooks.secret.ref", v))}
      description="Without it, Quai relies on polling alone."
    />
  )
}
