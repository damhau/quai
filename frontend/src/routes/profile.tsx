import { useState } from "react"
import { createFileRoute } from "@tanstack/react-router"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Loader2, Save } from "lucide-react"
import { toast } from "sonner"
import { getMeOptions, getPreferencesOptions, getPreferencesQueryKey, updatePreferencesMutation } from "@/api/generated/@tanstack/react-query.gen"
import type { PreferencesOutputBody } from "@/api/generated/types.gen"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Skeleton } from "@/components/ui/skeleton"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { bodyOf, browserTimeZone, channels, choiceKey, choicesOf, hours, kinds, modes, timeZones, type Choices, type Mode } from "@/lib/preferences"
import { isProblem, problemMessage } from "@/lib/problems"

export const Route = createFileRoute("/profile")({ component: Profile })

// The signed-in person's page: their notification preferences. Delegation (P4-13) goes here too.
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
          administrators of a repository always go at once.
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
