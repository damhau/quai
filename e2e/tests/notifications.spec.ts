import { expect, test } from "@playwright/test"
import { delivered, linkIn, mailFor, mailText, mailTo, newPage, preview, requestNamespace, signIn, unique } from "./helpers"

// Notifications by email (docs/design/05-delivery-and-approvals.md#notifications), as the local
// mail server (Mailpit) catches them.

test("the requester hears by email that their request was approved, merged and applied", async ({ page }) => {
  const name = `vm${unique()}`
  await signIn(page, "bob")
  await page.getByRole("link", { name: "Request Virtual machine (Terraform)" }).click()
  await page.getByRole("radio", { name: "dev" }).click()
  await page.getByRole("textbox", { name: /VM name/ }).fill(name)
  await expect(preview(page).getByText(`envs/dev/vms/${name}.yaml`)).toBeVisible()
  await page.getByRole("button", { name: "Submit and commit" }).click()
  await delivered(page)
  const id = new URL(page.url()).pathname.split("/").pop() ?? ""

  const target = `Create infra-live/vm/dev/${name}`
  await expect
    .poll(async () => (await mailTo("bob@example.com", target)).map((m) => m.Subject.replace(/#\d+ /, "")).sort(), { timeout: 60_000 })
    .toEqual([`[Quai] Applied: ${target}`, `[Quai] Approved: ${target}`, `[Quai] Merged: ${target}`])
  const [first] = await mailTo("bob@example.com", target)
  expect(await mailText(first?.ID ?? "")).toContain(`/requests/${id}`)
})

test("an approver hears by email that a request needs them, follows its link and approves", async ({ page, browser }) => {
  const { id, number, title } = await requestNamespace(page, `ns${unique()}`)

  const mail = await mailFor("alice@example.com", `Approval needed: #${number} `)
  expect(mail.Subject).toBe(`[Quai] Approval needed: #${number} ${title}`)
  expect(mail.text).toContain(`bob asks for your approval of request #${number}.`)
  expect(mail.text).toContain("Waiting for: type/platform (group:platform-admins)")
  const link = linkIn(mail.text)
  expect(link).toBe(`/requests/${id}`)

  // From her mailbox, signed in to nothing yet: the link asks her to sign in, then opens the request.
  const alice = await newPage(browser)
  try {
    await alice.goto(link)
    await expect(alice).toHaveURL(/\/login\?next=/)
    await alice.getByRole("button", { name: /^alice/ }).click()
    await expect(alice).toHaveURL(new RegExp(`/requests/${id}$`))
    await expect(alice.getByRole("heading", { name: new RegExp(title) })).toBeVisible()
    await alice.getByRole("button", { name: "Approve" }).click()
    await expect(alice.getByText(/^Approved by .*\balice\b/)).toBeVisible()
    await delivered(alice)
  } finally {
    await alice.context().close()
  }
  await mailFor("bob@example.com", `Approved: #${number} `)
})
