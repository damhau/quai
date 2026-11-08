import { expect, test, type Page } from "@playwright/test"
import { approve, delivered, fileOnGitea, onGitea, preview, pullRequest, signIn, unique } from "./helpers"

// Shared files (docs/design/02, entry and field): many resources in one file of the seeded
// repository. Namespaces are entries of clusters/<cluster>/namespaces.yaml, their quotas a field
// type over the same entries; DNS record sets are entries of an HCL map in
// dns/<zone>/records.auto.tfvars; databases are items of a JSON list, written by a mapping. Each
// change touches its entry's bytes only, and two requests on one file both merge.

/** changedLines returns the lines a diff adds or removes. */
const changedLines = (page: Page, path: string) =>
  page.getByRole("dialog").getByLabel(`Diff of ${path}`).locator("div").filter({ hasText: /^[-+]/ })

/** createNamespace requests a namespace on dev-1 as bob and has alice approve it. */
async function createNamespace(page: Page, name: string) {
  await signIn(page, "bob")
  await page.getByRole("link", { name: "Request Namespace" }).click()
  await page.getByRole("radio", { name: "dev-1" }).click()
  await page.getByRole("textbox", { name: "Name" }).fill(name)
  const diff = preview(page).getByLabel("Diff of clusters/dev-1/namespaces.yaml")
  await expect(diff).toContainText(`+  - name: ${name}`)
  await expect(diff).toContainText("+    owner: bob")
  await expect(preview(page).getByText(/type\/platform: 1 approval from/)).toBeVisible() // a new namespace
  await page.getByRole("button", { name: "Submit for approval" }).click()
  const title = `Create infra-live/namespace/dev-1/${name}`
  await expect(page.getByRole("heading", { name: new RegExp(title) })).toBeVisible()
  await approve(page, "alice", title)
  await delivered(page)
}

test("a namespace is created as an entry of its cluster's file, then edited in place", async ({ page }) => {
  const name = `ns${unique()}`
  await createNamespace(page, name)
  await expect(page.getByRole("list", { name: "Timeline" })).toContainText("the apply of clusters/dev-1 succeeded")

  // Its owner edits it: a description, on a development cluster, needs no approval.
  await signIn(page, "bob", `/resources/infra-live/namespace/dev-1/${name}`)
  await page.getByRole("button", { name: "Edit" }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog.getByRole("textbox", { name: "Name" })).toHaveCount(0) // the key mirrors the param
  await dialog.getByRole("textbox", { name: "Description" }).fill("The search team's sandbox")
  await expect(changedLines(page, "clusters/dev-1/namespaces.yaml")).toHaveText(["+    description: The search team's sandbox"])
  await expect(dialog.getByText(/No approval needed/)).toBeVisible()
  await dialog.getByRole("button", { name: "Submit and commit" }).click()
  await expect(page.getByRole("heading", { name: new RegExp(`Update infra-live/namespace/dev-1/${name}`) })).toBeVisible()
  await delivered(page)

  // The new key ends its entry (docs/design/03); the comments and the other entries are as they were.
  const file = await fileOnGitea("clusters/dev-1/namespaces.yaml")
  expect(file).toContain(`  - name: ${name}\n    owner: bob\n    quota:\n      cpu: 2\n      memory_gi: 4\n      pods: 50\n    description: The search team's sandbox\n`)
  expect(file).toContain("  # CI jobs of every team: short-lived pods, many of them.\n  - name: ci-runners\n")
  expect(file.startsWith("# Namespaces on the dev-1 cluster, one entry each;")).toBe(true)
})

test("a quota grows through the namespace-quota field type, which any developer may ask for", async ({ page }) => {
  const name = `q${unique()}`
  await createNamespace(page, name)

  // carol does not own bob's namespace, but the developers own its quota.
  await signIn(page, "carol", `/resources/infra-live/namespace/dev-1/${name}`)
  await expect(page.getByText("Only bob or an operator can change this resource.")).toBeVisible()
  await page.goto(`/resources/infra-live/namespace-quota/dev-1/${name}`)
  await expect(page.getByText("owned by team:developers")).toBeVisible()
  await page.getByRole("button", { name: "Edit" }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog.getByRole("textbox", { name: "Description" })).toHaveCount(0) // the quota only
  await dialog.getByRole("spinbutton", { name: "CPU (cores)" }).fill("6")
  await expect(changedLines(page, "clusters/dev-1/namespaces.yaml")).toHaveText(["-      cpu: 2", "+      cpu: 6"])
  await expect(dialog.getByText(/type\/platform: 1 approval from/)).toBeVisible() // the quota grows
  await dialog.getByRole("textbox", { name: "Reason" }).fill("the search index outgrew 2 cores")
  await dialog.getByRole("button", { name: "Submit for approval" }).click()
  const title = `Update infra-live/namespace-quota/dev-1/${name}`
  await expect(page.getByRole("heading", { name: new RegExp(title) })).toBeVisible()
  await approve(page, "alice", title)
  await delivered(page)

  await signIn(page, "bob", `/resources/infra-live/namespace/dev-1/${name}`)
  await expect(page.getByLabel("Content of clusters/dev-1/namespaces.yaml")).toContainText("cpu: 6")
  expect(await fileOnGitea("clusters/dev-1/namespaces.yaml")).toContain(`  - name: ${name}\n    owner: bob\n    quota:\n      cpu: 6\n      memory_gi: 4\n`)
})

/** requestRecord requests an A record set in shop.internal as the signed-in user, and returns
 * its title once its pull request is open. */
async function requestRecord(page: Page, name: string, ip: string): Promise<string> {
  await page.goto("/request/infra-live/types/dns-record")
  await page.getByRole("combobox", { name: "Zone" }).selectOption("shop.internal")
  await page.getByRole("textbox", { name: "Name" }).fill(name)
  await page.getByRole("button", { name: "Add to Values" }).click()
  await page.getByRole("textbox", { name: "Values 1" }).fill(ip)
  await expect(preview(page).getByLabel("Diff of dns/shop.internal/records.auto.tfvars")).toContainText(`+  ${name} = {`)
  await page.getByRole("button", { name: "Submit for approval" }).click()
  const title = `Create infra-live/dns-record/shop.internal/${name}`
  await expect(page.getByRole("heading", { name: new RegExp(title) })).toBeVisible()
  await pullRequest(page)
  return title
}

test("two requests on two entries of one file, approved together, are both merged", async ({ page, browser }, info) => {
  const u = unique()
  const records = [
    { name: `api${u}`, ip: "10.0.12.40" },
    { name: `cdn${u}`, ip: "10.0.12.41" },
  ]
  await signIn(page, "bob")
  const titles: string[] = []
  for (const r of records) titles.push(await requestRecord(page, r.name, r.ip))

  // Both pull requests are open; alice approves both at once, from two browsers. Each appends to
  // the records map: the second to merge is rebuilt on the first, and keeps its approval.
  const second = await browser.newContext({ baseURL: info.project.use.baseURL })
  const other = await second.newPage()
  await Promise.all([approve(page, "alice", titles[0]), approve(other, "alice", titles[1])])
  await Promise.all([delivered(page), delivered(other)])

  const timelines: string[] = []
  for (const p of [page, other]) {
    expect(await onGitea(await pullRequest(p))).toEqual({ merged: true, approval: "success" })
    timelines.push((await p.getByRole("list", { name: "Timeline" }).textContent()) ?? "")
  }
  expect(timelines.filter((t) => t.includes("rebuilt the pull request on the new base"))).toHaveLength(1)

  const file = await fileOnGitea("dns/shop.internal/records.auto.tfvars")
  for (const r of records) {
    expect(file).toContain(`  ${r.name} = {\n    owner  = "bob"\n    type   = "A"\n    ttl    = 300\n    values = ["${r.ip}"]\n  }\n`)
  }
  expect(file).toContain("  shop-db = {\n    owner  = \"bob\"\n    type   = \"A\"\n    ttl    = 24 * 3600\n")
  await second.close()
})

test("a value defined in code is shown read-only, and a change beside it keeps it", async ({ page }) => {
  await signIn(page, "bob", "/resources/infra-live/dns-record/shop.internal/shop-db")
  await page.getByRole("button", { name: "Edit" }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog.getByText("defined in code")).toBeVisible()
  await expect(dialog.getByText("24 * 3600", { exact: true })).toBeVisible()
  await expect(dialog.getByRole("spinbutton", { name: /TTL/ })).toHaveCount(0)

  // The database moves: its address changes, and its TTL stays an expression.
  const ip = dialog.getByRole("textbox", { name: "Values 1" })
  const from = await ip.inputValue()
  const to = from === "10.0.13.31" ? "10.0.13.32" : "10.0.13.31"
  await ip.fill(to)
  await expect(changedLines(page, "dns/shop.internal/records.auto.tfvars")).toHaveText([`-    values = ["${from}"]`, `+    values = ["${to}"]`])
  await dialog.getByRole("button", { name: "Submit for approval" }).click()
  const title = "Update infra-live/dns-record/shop.internal/shop-db"
  await expect(page.getByRole("heading", { name: new RegExp(title) })).toBeVisible()
  await approve(page, "alice", title)
  await delivered(page)

  expect(await fileOnGitea("dns/shop.internal/records.auto.tfvars")).toContain(
    `  # Clients cache the database's address for a day: it moves only with a migration.\n  shop-db = {\n    owner  = "bob"\n    type   = "A"\n    ttl    = 24 * 3600\n    values = ["${to}"]\n  }\n`,
  )
})

test("a database of the mapping type is sized from the form, and resized in place", async ({ page }) => {
  const name = `db${unique()}`
  const path = "databases/dev/databases.auto.tfvars.json"
  await signIn(page, "bob")
  await page.getByRole("link", { name: "Request Database" }).click()
  await page.getByRole("radio", { name: "dev", exact: true }).click()
  await page.getByRole("textbox", { name: "Name" }).fill(name)
  await expect(preview(page).getByLabel(`Diff of ${path}`)).toContainText('"vcpu": 2')
  await expect(preview(page).getByText(/No approval needed/)).toBeVisible()
  await page.getByRole("button", { name: "Submit and commit" }).click()
  await expect(page.getByRole("heading", { name: new RegExp(`Create infra-live/database/dev/${name}`) })).toBeVisible()
  await delivered(page)

  // The edit form starts from the size read back from the file; the template rewrites three values.
  await page.goto(`/resources/infra-live/database/dev/${name}`)
  await page.getByRole("button", { name: "Edit" }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog.getByRole("radio", { name: "small" })).toBeChecked()
  await dialog.getByRole("radio", { name: "medium" }).click()
  await expect(changedLines(page, path)).toHaveText([
    '-      "vcpu": 2,',
    '-      "memory_gb": 4,',
    '-      "storage_gb": 20',
    '+      "vcpu": 4,',
    '+      "memory_gb": 16,',
    '+      "storage_gb": 100',
  ])
  await dialog.getByRole("button", { name: "Submit and commit" }).click()
  await expect(page.getByRole("heading", { name: new RegExp(`Update infra-live/database/dev/${name}`) })).toBeVisible()
  await delivered(page)
  await page.goto(`/resources/infra-live/database/dev/${name}`)
  await expect(page.getByLabel(`Content of ${path}`)).toContainText('"storage_gb": 100')
})
