import { readFileSync } from "node:fs"
import { expect, test, type Browser, type Page } from "@playwright/test"
import { approvalOnGitea, approve, csrf, delivered, linkIn, mailFor, mailTo, newPage, onGitea, pullRequest, requestNamespace, signIn, unique, type User } from "./helpers"

// Governance (docs/design/05-delivery-and-approvals.md, docs/design/09-security-and-compliance.md)
// on the compose stack: a delegation of approvals, a freeze on the seeded repository, break-glass,
// and an auditor's evidence. Each scenario makes its own request on a namespace of dev-1, which
// waits for a platform admin (alice); what a scenario changes beyond it, a delegation or a freeze,
// is undone after it, whether it passed or not.

/** What the running scenario changed beyond its request, undone after it, last first. */
const undo: ((browser: Browser) => Promise<void>)[] = []

test.afterEach(async ({ browser }) => {
  for (const step of undo.splice(0).reverse()) await step(browser)
})

/** as runs steps on a page of its own, signed in as user, then closes it. */
async function as(browser: Browser, user: User, steps: (page: Page) => Promise<void>) {
  const page = await newPage(browser)
  try {
    await signIn(page, user)
    await steps(page)
  } finally {
    await page.context().close()
  }
}

/** revokeDelegations revokes the delegations the person signed in on page gave, as their
 * profile's Revoke does. */
async function revokeDelegations(page: Page, delegator: User) {
  const res = await page.request.get("/api/v1/me/delegations")
  expect(res.ok(), `GET /api/v1/me/delegations: ${res.status()}`).toBe(true)
  const { delegations } = (await res.json()) as { delegations: { id: string; delegator: string }[] | null }
  for (const d of (delegations ?? []).filter((d) => d.delegator === delegator)) {
    const del = await page.request.delete(`/api/v1/delegations/${d.id}`, { headers: { "X-CSRF-Token": await csrf(page) } })
    expect(del.ok(), `DELETE /api/v1/delegations/${d.id}: ${del.status()}`).toBe(true)
  }
}

test("an approver away delegates their approvals; the delegate approves on their behalf, and the request says so", async ({ page }) => {
  // One delegates to someone who has signed in to Quai.
  await signIn(page, "fiona")
  await signIn(page, "alice", "/profile")
  undo.push((browser) => as(browser, "alice", (p) => revokeDelegations(p, "alice")))
  await page.getByRole("textbox", { name: "Colleague" }).fill("fiona")
  await page.getByRole("button", { name: "Delegate" }).click()
  const given = page.getByRole("list", { name: "Your delegations" })
  await expect(given).toContainText("To fiona")
  await expect(given).toContainText("Active")

  const { number, title } = await requestNamespace(page, `ns${unique()}`)
  // The approval needed goes to the delegate, in alice's place, and says whose it is.
  const mail = await mailFor("fiona@example.com", `Approval needed: #${number} `)
  expect(mail.text).toContain(`bob asks for your approval of request #${number}. You hold alice's approvals through a delegation: you review it on their behalf.`)
  expect(await mailTo("alice@example.com", `Approval needed: #${number} `)).toEqual([])

  await signIn(page, "fiona", "/approvals")
  await page.getByRole("link", { name: title }).click()
  await expect(page.getByText("You review it on behalf of alice, who delegated their approvals to you.")).toBeVisible()
  await page.getByRole("button", { name: "Approve" }).click()
  const reviews = page.getByRole("list", { name: "Reviews" })
  await expect(reviews).toContainText("fiona approved on behalf of alice")
  await expect(reviews).toContainText("through a delegation")
  await delivered(page)
  await expect(page.getByText("Approved by fiona (on behalf of alice)", { exact: true })).toBeVisible()
  await expect(page.getByRole("list", { name: "Timeline" })).toContainText("fiona approved on behalf of alice")

  // Back, alice revokes it.
  await signIn(page, "alice", "/profile")
  await page.getByRole("button", { name: "Revoke the delegation to fiona" }).click()
  await expect(page.getByText("You have not delegated your approvals.")).toBeVisible()
})

/** A change window of a repository document (docs/design/01-repository-connections.md). */
type ChangeWindow = { name: string; allow?: string[]; deny?: string[]; when?: string }

type Document = { spec: { enforcement?: { changeWindows?: ChangeWindow[] } } }

/** changeWindows changes infra-live's change windows as a platform admin does, signed in on page:
 * the connection is managed in Quai (the compose stack connects it through the API), so its
 * document is read and written back through the API. */
async function changeWindows(page: Page, change: (windows: ChangeWindow[]) => ChangeWindow[]) {
  const res = await page.request.get("/api/v1/repositories/infra-live")
  expect(res.ok(), `GET /api/v1/repositories/infra-live: ${res.status()}`).toBe(true)
  const { managedBy, document } = (await res.json()) as { managedBy: string; document: Document }
  expect(managedBy).toBe("api")
  const before = document.spec.enforcement?.changeWindows ?? []
  const after = change(before)
  if (JSON.stringify(after) === JSON.stringify(before)) return
  document.spec.enforcement = { ...document.spec.enforcement, changeWindows: after.length > 0 ? after : undefined }
  const put = await page.request.put("/api/v1/repositories/infra-live", { data: document, headers: { "X-CSRF-Token": await csrf(page) } })
  expect(put.ok(), `PUT /api/v1/repositories/infra-live: ${put.status()} ${await put.text()}`).toBe(true)
}

test("a freeze on the repository holds an approved request, which merges once the freeze is lifted", async ({ page }) => {
  // Released by the windows' sweep, which runs every minute, then merged and applied.
  test.setTimeout(240_000)
  const freeze = `freeze-${unique()}`
  const { title } = await requestNamespace(page, `ns${unique()}`)
  const number = await pullRequest(page)

  await signIn(page, "alice")
  undo.push((browser) => as(browser, "alice", (p) => changeWindows(p, (ws) => ws.filter((w) => w.name !== freeze))))
  // From yesterday to the day after tomorrow, in UTC: now, whatever the hour of the run.
  const day = (offset: number) => new Date(Date.now() + offset * 86_400_000).toISOString().slice(0, 10)
  await changeWindows(page, (ws) => [...ws, { name: freeze, deny: [`${day(-1)} to ${day(2)} UTC`] }])

  await approve(page, "alice", title)
  await expect(page.getByTestId("change-window")).toContainText(`Approved; waits for its change windows (${freeze})`)
  await expect(page.getByRole("list", { name: "Timeline" })).toContainText(`waits for its change windows (${freeze})`)
  await approvalOnGitea(number, "pending")
  expect(await onGitea(number)).toEqual({ merged: false, approval: "pending" })

  await changeWindows(page, (ws) => ws.filter((w) => w.name !== freeze))
  await expect(page.getByText("Applied", { exact: true })).toBeVisible({ timeout: 150_000 })
  await expect(page.getByRole("list", { name: "Timeline" })).toContainText("its change windows opened")
  expect(await onGitea(number)).toEqual({ merged: true, approval: "success" })
})

test("break-glass merges a pending request for an incident, alerts, and its review closes on two sign-offs", async ({ page }) => {
  const { id, number } = await requestNamespace(page, `ns${unique()}`)
  const pr = await pullRequest(page)
  const incident = `INC${Date.now().toString().slice(-7)}`
  const reason = "the search outage needs its namespace before the approvers are back"

  // fiona is on call, with the break-glass role.
  await signIn(page, "fiona", `/requests/${id}`)
  await page.getByRole("button", { name: "Break glass" }).click()
  const dialog = page.getByRole("dialog")
  await dialog.getByRole("textbox", { name: "Incident" }).fill(incident)
  await dialog.getByRole("textbox", { name: "Why it cannot wait for its approvals" }).fill(reason)
  await dialog.getByRole("button", { name: "Break glass and merge" }).click()
  const card = page.getByRole("region", { name: "Break-glass" })
  await expect(card).toContainText("Break-glass by fiona")
  await expect(card).toContainText(`Merged without its approvals, for incident ${incident}.`)
  await delivered(page)
  expect((await onGitea(pr)).merged).toBe(true)

  // The alert, to the platform's alert address and the namespace's owner; never the reason.
  const alert = await mailFor("platform-team@example.com", `Alert: break-glass on request #${number} `)
  expect(alert.Subject).toBe(`[Quai] Alert: break-glass on request #${number} in infra-live`)
  for (const line of ["State then: pending_approval", "Broken by: fiona", `Incident: ${incident}`]) expect(alert.text).toContain(line)
  expect(alert.text).not.toContain(reason)
  expect(linkIn(alert.text)).toBe(`/requests/${id}`)
  await mailFor("bob@example.com", `Alert: break-glass on request #${number} `)

  // The post-incident review: neither the person who broke glass nor the requester signs it off.
  await expect(card).toContainText("Open: 0 of 2 sign-offs")
  await expect(card.getByRole("button", { name: "Sign off the review" })).toHaveCount(0)
  const signOffs = [
    ["alice", "a platform admin", "the namespace was needed for the outage, as the incident records"],
    ["erin", "an auditor", "the audit log matches the incident's timeline"],
  ] as const
  for (const [user, capacity, found] of signOffs) {
    await signIn(page, user, `/requests/${id}`)
    await card.getByRole("textbox", { name: "What the review found" }).fill(found)
    await card.getByRole("button", { name: "Sign off the review" }).click()
    await expect(card.getByRole("list", { name: "Sign-offs" })).toContainText(`${user} signed it off as ${capacity}`)
    if (user === "alice") await expect(card).toContainText("Open: 1 of 2 sign-offs")
  }
  await expect(card).toContainText(/Closed on .+: signed off by alice and erin\./)
})

/** What the scenario reads of an evidence bundle (docs/design/09-security-and-compliance.md#evidence-bundle). */
type Bundle = {
  request: { id: string; number: number; state: string; requester: string }
  approvals: { reviewer: string; decision: string }[]
  events: { seq: number; action: string; object: { id: string; type: string } }[]
  proof: { segments: unknown[]; unproven: { seq: number; reason: string }[]; omitted?: string }
}

test("an auditor reads a request's audit events, samples it and checks its evidence bundle", async ({ page }) => {
  const { id, number, title } = await requestNamespace(page, `ns${unique()}`)
  await approve(page, "alice", title)
  await delivered(page)

  // erin audits: the audit log, narrowed to the request.
  await signIn(page, "erin")
  const nav = page.getByRole("navigation", { name: "Main" })
  await nav.getByRole("link", { name: "Audit log" }).click()
  await page.getByRole("textbox", { name: "About" }).fill(id)
  await page.getByRole("button", { name: "Filter" }).click()
  const events = page.getByRole("table", { name: "Audit events" }).getByRole("row").filter({ hasText: /request\.\w+/ })
  const actions = ["request.submitted", "request.reviewed", "request.approved", "request.merged", "request.applied"]
  for (const action of actions) await expect(events.filter({ hasText: action })).toHaveCount(1)
  await expect(events).toHaveCount(actions.length)
  const seqs = (await events.locator("td:first-child").allTextContents()).map(Number)
  // The compose stack has no signing key (audit.checkpoints.signingKey): nothing proves the log yet.
  await expect(page.getByText("No signed checkpoint yet.")).toBeVisible()

  // A sample: infra-live's applied requests of the last 30 days, and the request's bundle.
  await nav.getByRole("link", { name: "Evidence" }).click()
  await page.getByLabel("Repository").selectOption({ label: "Infrastructure (Terraform)" })
  await page.getByLabel("State").selectOption({ label: "Applied" })
  await page.getByRole("button", { name: "List" }).click()
  const row = page.getByRole("table", { name: "Requests" }).getByRole("row").filter({ has: page.getByRole("link", { name: `#${number}`, exact: true }) })
  await expect(row).toContainText("Applied")
  await expect(row).toContainText("bob")
  const [download] = await Promise.all([page.waitForEvent("download"), row.getByRole("link", { name: `JSON bundle of #${number}` }).click()])
  expect(download.suggestedFilename()).toBe(`quai-request-${number}-evidence.json`)
  const bundle = JSON.parse(readFileSync(await download.path(), "utf8")) as Bundle

  expect(bundle.request).toMatchObject({ id, number, state: "applied", requester: "bob" })
  expect(bundle.approvals).toEqual([expect.objectContaining({ reviewer: "alice", decision: "approve" })])
  // The request's own events, those the audit log showed among them.
  expect(bundle.events.every((e) => e.object.type === "request" && e.object.id === id)).toBe(true)
  expect(bundle.events.map((e) => e.action)).toEqual(expect.arrayContaining(actions))
  expect(bundle.events.map((e) => e.seq)).toEqual(expect.arrayContaining(seqs))
  // No proof: an auditor's bundle leaves none out, but no checkpoint signs the events, so none
  // is proven, each for that reason.
  const bySeq = (a: number, b: number) => a - b
  expect(bundle.proof.omitted).toBeUndefined()
  expect(bundle.proof.segments).toEqual([])
  expect(bundle.proof.unproven.map((u) => u.seq).sort(bySeq)).toEqual(bundle.events.map((e) => e.seq).sort(bySeq))
  for (const u of bundle.proof.unproven) expect(u.reason).toContain("the audit log has no signed checkpoint")
})
