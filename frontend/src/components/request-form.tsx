import type { ReactNode } from "react"
import { Link } from "@tanstack/react-router"
import type { UseQueryResult } from "@tanstack/react-query"
import { GitCommitHorizontal, Loader2, ShieldCheck } from "lucide-react"
import type { PlanView } from "@/api/generated/types.gen"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"

// The parts the request pages share: their header, the plain fields outside the schema (locator
// params, instance name), the reason, and the submit button.

export function RequestHeader({ title, description, children }: { title: string; description?: string; children?: ReactNode }) {
  return (
    <header className="grid gap-1">
      <nav aria-label="Breadcrumb" className="text-muted-foreground text-sm">
        <Link to="/" className="hover:text-foreground">
          Catalog
        </Link>{" "}
        / {title}
      </nav>
      <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
      {description && <p className="text-muted-foreground">{description}</p>}
      {children}
    </header>
  )
}

export function TextField({
  id,
  label,
  description,
  required,
  value,
  error,
  onChange,
  onBlur,
  disabled,
  placeholder,
}: {
  id: string
  label: string
  description?: string
  required?: boolean
  value: string
  error?: string
  onChange: (v: string) => void
  onBlur?: () => void
  disabled?: boolean
  placeholder?: string
}) {
  const describedBy = [description && `${id}-desc`, error && `${id}-error`].filter(Boolean).join(" ") || undefined
  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>
        {label}
        {required && (
          <span className="text-destructive" aria-hidden>
            *
          </span>
        )}
      </Label>
      <Input
        id={id}
        value={value}
        autoComplete="off"
        spellCheck={false}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy}
        disabled={disabled}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        onBlur={onBlur}
      />
      {description && (
        <p id={`${id}-desc`} className="text-muted-foreground text-xs">
          {description}
        </p>
      )}
      {error && (
        <p id={`${id}-error`} className="text-destructive text-xs">
          {error}
        </p>
      )}
    </div>
  )
}

export function ReasonField({ value, onChange, needsApproval, error }: { value: string; onChange: (v: string) => void; needsApproval: boolean; error?: string }) {
  return (
    <div className="grid gap-2">
      <Label htmlFor="reason">Reason</Label>
      <Textarea
        id="reason"
        rows={3}
        maxLength={2000}
        value={value}
        aria-invalid={error ? true : undefined}
        placeholder={needsApproval ? "Approvers will read this" : "Optional; it ends up in the commit message"}
        onChange={(e) => onChange(e.target.value)}
      />
      {error && <p className="text-destructive text-xs">{error}</p>}
    </div>
  )
}

/** SubmitButton is enabled once the form plans cleanly, and says whether approval comes next. */
export function SubmitButton({ plan, pending }: { plan: UseQueryResult<PlanView, unknown>; pending: boolean }) {
  const needsApproval = (plan.data?.rules?.length ?? 0) > 0
  return (
    <Button type="submit" disabled={!plan.data || plan.isError || plan.isFetching || pending} className="w-fit">
      {pending ? <Loader2 className="animate-spin" /> : needsApproval ? <ShieldCheck /> : <GitCommitHorizontal />}
      {needsApproval ? "Submit for approval" : "Submit and commit"}
    </Button>
  )
}
