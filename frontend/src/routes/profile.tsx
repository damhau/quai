import { useState } from "react"
import { createFileRoute } from "@tanstack/react-router"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Loader2, Save, UserCheck } from "lucide-react"
import { toast } from "sonner"
import {
  createDelegationMutation,
  getMeOptions,
  getPreferencesOptions,
  getPreferencesQueryKey,
  listMyDelegationsOptions,
  listMyDelegationsQueryKey,
  revokeDelegationMutation,
  updatePreferencesMutation,
} from "@/api/generated/@tanstack/react-query.gen"
import type { DelegationView, PreferencesOutputBody } from "@/api/generated/types.gen"
import { StepUpPrompt, StepUpRefusal } from "@/components/step-up-prompt"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { instantOf, localInput, when } from "@/lib/delegations"
import { bodyOf, browserTimeZone, channels, choiceKey, choicesOf, hours, kinds, modes, timeZones, type Choices, type Mode } from "@/lib/preferences"
import { fieldErrors, hasFieldErrors, isProblem, isStepUpRequired, problemMessage } from "@/lib/problems"

export const Route = createFileRoute("/profile")({ component: Profile })

// The signed-in person's page: their notification preferences, and the delegations of approvals
// they gave or received.
function Profile() {
  const { data: me } = useQuery(getMeOptions())
  return (
    <section aria-labelledby="profile-title" className="grid max-w-4xl gap-6">
      <header className="grid gap-1">
        <h1 id="profile-title" className="text-2xl font-semibold tracking-tight">
          Profile
        </h1>
        {me && (
          <p className="text-muted-foreground">
            {me.name}
            {me.email ? ` · ${me.email}` : ""}
          </p>
        )}
      </header>
      <Notifications email={me?.email} />
      {me && <Delegation me={me.name} />}
    </section>
  )
}

function Notifications({ email }: { email?: string }) {
  const prefs = useQuery(getPreferencesOptions())
  // Email and Slack find people by their verified email.
  const byEmail = ([["email", "email"], ["slack", "Slack message"]] as const)
    .filter(([c]) => (prefs.data?.channels ?? []).includes(c))
    .map(([, what]) => what)
  return (
    <Card>
      <CardHeader>
        <CardTitle>
          <h2 id="notifications-title">Notifications</h2>
        </CardTitle>
        <CardDescription>
          For each kind of notification, whether Quai sends it at once, in a daily digest, or not at all. Alerts to the
          administrators of a repository always go at once, and so do escalations of requests that wait too long for approval.
          Reminders follow your choice for Approval needed.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-6">
        {prefs.isPending && <Skeleton className="h-64" />}
        {prefs.error && (
          <Alert variant="destructive">
            <AlertDescription>{problemMessage(prefs.error)}</AlertDescription>
          </Alert>
        )}
        {prefs.data && (prefs.data.channels ?? []).length === 0 && (
          <p className="text-muted-foreground">Quai sends no notifications: no channel is configured.</p>
        )}
        {prefs.data && (prefs.data.channels ?? []).length > 0 && (
          <>
            {byEmail.length > 0 && !email && (
              <Alert>
                <AlertTitle>No email</AlertTitle>
                <AlertDescription>
                  Your identity provider gives Quai no verified email of yours: no {byEmail.join(" or ")} reaches you, whatever
                  you choose here.
                </AlertDescription>
              </Alert>
            )}
            {/* A new form when the server's preferences change: after a save, they are what it kept. */}
            <PreferencesForm key={JSON.stringify(prefs.data)} view={prefs.data} />
          </>
        )}
      </CardContent>
    </Card>
  )
}

function PreferencesForm({ view }: { view: PreferencesOutputBody }) {
  const [choices, setChoices] = useState<Choices>(() => choicesOf(view))
  const [hour, setHour] = useState(view.digest.hour)
  const [timeZone, setTimeZone] = useState(view.digest.timeZone)
  const browser = browserTimeZone()
  const queryClient = useQueryClient()
  const save = useMutation({
    ...updatePreferencesMutation(),
    onSuccess: (data) => {
      queryClient.setQueryData(getPreferencesQueryKey(), data)
      toast.success("Your notification preferences are saved")
    },
  })
  const list = view.channels ?? []
  const events = view.events ?? []
  const anyDigest = Object.values(choices).includes("digest")
  const reset = () => {
    setChoices(choicesOf(view, true))
    setHour(view.defaultDigest.hour)
    setTimeZone(view.defaultDigest.timeZone)
  }
  return (
    <form
      aria-labelledby="notifications-title"
      className="grid gap-6"
      onSubmit={(e) => {
        e.preventDefault()
        save.mutate({ body: bodyOf(view, choices, hour, timeZone) })
      }}
    >
      <Table aria-label="Notification preferences">
        <TableHeader>
          <TableRow>
            <TableHead>Notification</TableHead>
            {list.map((c) => (
              <TableHead key={c}>{channels[c] ?? c}</TableHead>
            ))}
          </TableRow>
        </TableHeader>
        <TableBody>
          {Object.entries(kinds).map(([kind, { label, description }]) => (
            <TableRow key={kind} className="align-top">
              <TableCell className="whitespace-normal">
                <div className="font-medium">{label}</div>
                <div className="text-muted-foreground text-xs">{description}</div>
              </TableCell>
              {list.map((channel) => {
                const e = events.find((x) => x.kind === kind && x.channel === channel)
                if (!e) return <TableCell key={channel} />
                return (
                  <TableCell key={channel}>
                    <NativeSelect
                      size="sm"
                      aria-label={`${label} by ${channels[channel] ?? channel}`}
                      value={choices[choiceKey(kind, channel)] ?? e.mode}
                      onChange={(ev) => setChoices({ ...choices, [choiceKey(kind, channel)]: ev.target.value as Mode })}
                    >
                      {(e.modes ?? []).map((m) => (
                        <NativeSelectOption key={m} value={m}>
                          {modes[m as Mode] ?? m}
                          {m === e.default ? " (default)" : ""}
                        </NativeSelectOption>
                      ))}
                    </NativeSelect>
                  </TableCell>
                )
              })}
            </TableRow>
          ))}
        </TableBody>
      </Table>
      {list.length === 1 && (
        <p className="text-muted-foreground text-sm">
          Approvals cannot be turned off: requests that wait for you must reach you, at once or in your digest.
        </p>
      )}

      <fieldset className="grid gap-3">
        <legend className="mb-2 font-medium">Daily digest</legend>
        <p className="text-muted-foreground text-sm">
          {anyDigest
            ? "One message a day with what you chose to receive in it, on days when there is something."
            : "Nothing goes in a digest: choose “In the daily digest” above for the notifications you want gathered."}
        </p>
        <div className="flex flex-wrap items-end gap-4">
          <div className="grid gap-1.5">
            <Label htmlFor="digest-hour">Send it at</Label>
            <NativeSelect id="digest-hour" value={hour} onChange={(e) => setHour(Number(e.target.value))}>
              {hours.map((h) => (
                <NativeSelectOption key={h.value} value={h.value}>
                  {h.label}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="digest-zone">Time zone</Label>
            <NativeSelect id="digest-zone" value={timeZone} onChange={(e) => setTimeZone(e.target.value)}>
              {timeZones(timeZone, view.defaultDigest.timeZone).map((z) => (
                <NativeSelectOption key={z} value={z}>
                  {z}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </div>
          {browser && browser !== timeZone && (
            <Button type="button" variant="outline" onClick={() => setTimeZone(browser)}>
              Use {browser}
            </Button>
          )}
        </div>
      </fieldset>

      {save.error && (
        <Alert variant="destructive">
          <AlertTitle>Not saved</AlertTitle>
          <AlertDescription>
            {problemMessage(save.error)}
            {isProblem(save.error) && (save.error.errors ?? []).length > 0 && (
              <ul className="mt-1 list-disc pl-5">
                {(save.error.errors ?? []).map((d, i) => (
                  <li key={i}>{d.message}</li>
                ))}
              </ul>
            )}
          </AlertDescription>
        </Alert>
      )}
      <div className="flex flex-wrap gap-2">
        <Button type="submit" disabled={save.isPending}>
          {save.isPending ? <Loader2 className="animate-spin" /> : <Save />}
          Save
        </Button>
        <Button type="button" variant="outline" onClick={reset}>
          Back to the defaults
        </Button>
      </div>
    </form>
  )
}

/** delegationTitle is what the profile says when delegating needs a recent sign-in. */
const delegationTitle = "Delegating your approvals needs a recent sign-in"

/**
 * Delegation hands the person's approvals to a colleague for a period, out of office
 * (docs/design/05-delivery-and-approvals.md#delegation), and lists the delegations they gave,
 * which they revoke, and received. Times are the browser's; the server takes instants. Nothing
 * here names the delegator: the server takes them from the session.
 */
function Delegation({ me }: { me: string }) {
  const list = useQuery(listMyDelegationsOptions())
  const all = list.data?.delegations ?? []
  const given = all.filter((d) => d.delegator === me)
  const received = all.filter((d) => d.delegate === me)
  return (
    <Card>
      <CardHeader>
        <CardTitle>
          <h2 id="delegation-title">Delegation</h2>
        </CardTitle>
        <CardDescription>
          While you are away, a colleague may approve or deny on your behalf what you could approve or deny yourself. Your
          approvals needed go to them; alerts still reach you. Their reviews name you both and count once. You can revoke a
          delegation at any time.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-6">
        {list.isPending && <Skeleton className="h-24" />}
        {list.error && (
          <Alert variant="destructive">
            <AlertDescription>{problemMessage(list.error)}</AlertDescription>
          </Alert>
        )}
        {list.data && (
          <>
            <DelegationList title="Your delegations" empty="You have not delegated your approvals." list={given} revocable />
            {received.length > 0 && <DelegationList title="Delegated to you" empty="" list={received} />}
            <DelegationForm maxDays={list.data.maxDays} stepUp={list.data.stepUp} />
          </>
        )}
      </CardContent>
    </Card>
  )
}

function DelegationList({ title, empty, list, revocable }: { title: string; empty: string; list: DelegationView[]; revocable?: boolean }) {
  const queryClient = useQueryClient()
  const revoke = useMutation({
    ...revokeDelegationMutation(),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: listMyDelegationsQueryKey() })
      toast.success("The delegation is revoked")
    },
  })
  return (
    <div className="grid gap-2">
      <h3 className="font-medium">{title}</h3>
      {list.length === 0 ? (
        <p className="text-muted-foreground text-sm">{empty}</p>
      ) : (
        <ul className="grid gap-2" aria-label={title}>
          {list.map((d) => (
            <li key={d.id} className="flex flex-wrap items-center gap-2 text-sm">
              <UserCheck className="text-muted-foreground size-4" aria-hidden />
              <span>
                {revocable ? (
                  <>
                    To <span className="font-medium">{d.delegate}</span>
                  </>
                ) : (
                  <>
                    From <span className="font-medium">{d.delegator}</span>: you may review on their behalf what they could review
                  </>
                )}
                , {when(d.startsAt)} until {when(d.endsAt)}
              </span>
              <Badge variant={d.active ? "default" : "outline"}>{d.active ? "Active" : "Scheduled"}</Badge>
              {revocable && (
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  aria-label={`Revoke the delegation to ${d.delegate}`}
                  disabled={revoke.isPending}
                  onClick={() => revoke.mutate({ path: { id: d.id } })}
                >
                  Revoke
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}
      {revoke.error && (
        <Alert variant="destructive">
          <AlertDescription>{problemMessage(revoke.error)}</AlertDescription>
        </Alert>
      )}
    </div>
  )
}

function DelegationForm({ maxDays, stepUp }: { maxDays: number; stepUp: boolean }) {
  const [delegate, setDelegate] = useState("")
  const [from, setFrom] = useState("") // empty: now
  const [until, setUntil] = useState(() => localInput(new Date(Date.now() + 7 * 24 * 3600 * 1000)))
  const zone = browserTimeZone()
  const queryClient = useQueryClient()
  const { data: me } = useQuery(getMeOptions())
  const create = useMutation({
    ...createDelegationMutation(),
    onSuccess: async (d) => {
      await queryClient.invalidateQueries({ queryKey: listMyDelegationsQueryKey() })
      setDelegate("")
      setFrom("")
      toast.success(`Your approvals are delegated to ${d.delegate}`)
    },
  })
  const errors = fieldErrors(create.error, "body")
  const needsSignIn = stepUp && me && !me.stepUp.satisfied
  return (
    <form
      aria-labelledby="delegate-title"
      className="grid gap-3 border-t pt-4"
      onSubmit={(e) => {
        e.preventDefault()
        create.mutate({ body: { delegate: delegate.trim(), startsAt: instantOf(from) ?? new Date().toISOString(), endsAt: instantOf(until) ?? "" } })
      }}
    >
      <h3 id="delegate-title" className="font-medium">
        Delegate your approvals
      </h3>
      <p className="text-muted-foreground text-sm">
        To someone who has signed in to Quai, for {maxDays} days at most; one delegate at a time. Times are in your time zone
        {zone ? ` (${zone})` : ""}.
      </p>
      <div className="flex flex-wrap items-start gap-4">
        <div className="grid gap-1.5">
          <Label htmlFor="delegate-name">Colleague</Label>
          <Input
            id="delegate-name"
            required
            maxLength={256}
            placeholder="Their name in Quai"
            value={delegate}
            aria-invalid={errors.delegate ? true : undefined}
            onChange={(e) => setDelegate(e.target.value)}
          />
          {errors.delegate && <p className="text-destructive text-xs">{errors.delegate}</p>}
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor="delegate-from">From</Label>
          <Input
            id="delegate-from"
            type="datetime-local"
            value={from}
            aria-describedby="delegate-from-hint"
            aria-invalid={errors.startsAt ? true : undefined}
            onChange={(e) => setFrom(e.target.value)}
          />
          <p id="delegate-from-hint" className="text-muted-foreground text-xs">
            Empty: now
          </p>
          {errors.startsAt && <p className="text-destructive text-xs">{errors.startsAt}</p>}
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor="delegate-until">Until</Label>
          <Input
            id="delegate-until"
            type="datetime-local"
            required
            value={until}
            aria-invalid={errors.endsAt ? true : undefined}
            onChange={(e) => setUntil(e.target.value)}
          />
          {errors.endsAt && <p className="text-destructive text-xs">{errors.endsAt}</p>}
        </div>
      </div>
      {create.error && isStepUpRequired(create.error) ? (
        <StepUpRefusal error={create.error} reset={() => create.reset()} title={delegationTitle} />
      ) : (
        create.error &&
        !hasFieldErrors(create.error) && (
          <Alert variant="destructive">
            <AlertTitle>Not delegated</AlertTitle>
            <AlertDescription>{problemMessage(create.error)}</AlertDescription>
          </Alert>
        )
      )}
      {needsSignIn && !create.error && <StepUpPrompt title={delegationTitle} reason={me.stepUp.reason} />}
      <div>
        <Button type="submit" disabled={create.isPending || !!needsSignIn}>
          {create.isPending ? <Loader2 className="animate-spin" /> : <UserCheck />}
          Delegate
        </Button>
      </div>
    </form>
  )
}
