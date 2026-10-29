import { expect, test } from "@playwright/test"
import { approve, delivered, preview, pullRequest, signIn, unique } from "./helpers"

// The repository's pipelines are the stack's fake runner (docker-compose.yml): it plans each pull
// request and applies main, reporting with quai report. A request's reason steers it: "[costly]"
// makes the plan cost 900 CHF a month, "[fail-apply]" fails the apply. The seeded vm type has a
// rule on the plan's cost: a production VM costing more than 500 a month needs FinOps too.

test("a costly plan brings FinOps in, and the request and its VM are applied after the merge", async ({ page }) => {
  const name = `big${unique()}`
  await signIn(page, "bob")
  await page.getByRole("link", { name: "Request Virtual machine (Terraform)" }).click()
  await page.getByRole("radio", { name: "prod" }).click()
  await page.getByRole("textbox", { name: /VM name/ }).fill(name)
  await expect(preview(page).getByText("Decided by the plan:")).toBeVisible()
  await page.getByRole("textbox", { name: "Reason" }).fill("[costly] a bigger shop")
  await page.getByRole("button", { name: "Submit for approval" }).click()
  const title = `Create infra-live/vm/prod/${name}`
  await expect(page.getByRole("heading", { name: new RegExp(title) })).toBeVisible()
  await pullRequest(page)

  // The plan shows while the approvals wait; it costs more than 500 a month: the rule applies.
  const plan = page.getByRole("region", { name: "Plan of envs/prod" })
  await expect(plan).toBeVisible({ timeout: 60_000 })
  await expect(plan.getByText("+CHF 900/month")).toBeVisible()
  await expect(page.getByText("Applies to the plan:")).toBeVisible()
  await expect(page.getByRole("list", { name: "Timeline" })).toContainText("the plan of envs/prod succeeded")

  await approve(page, "alice", title)
  await expect(page.getByText("Pending approval", { exact: true })).toBeVisible() // FinOps, for the cost
  await approve(page, "fiona", title)
  await delivered(page)
  await expect(page.getByRole("list", { name: "Timeline" })).toContainText("the apply of envs/prod succeeded")

  await page.goto(`/resources/infra-live/vm/prod/${name}`)
  await expect(page.getByText("Applied", { exact: true }).first()).toBeVisible({ timeout: 30_000 })
  await expect(page.getByRole("list", { name: "Applies" })).toContainText("the apply of envs/prod succeeded")
})

test("a failed apply fails the request and the VM", async ({ page }) => {
  const name = `bad${unique()}`
  await signIn(page, "bob")
  await page.getByRole("link", { name: "Request Virtual machine (Terraform)" }).click()
  await page.getByRole("radio", { name: "dev" }).click()
  await page.getByRole("textbox", { name: /VM name/ }).fill(name)
  // In dev, the rule on the cost is decided without a plan: it does not apply.
  await expect(preview(page).getByText(/No approval needed/)).toBeVisible()
  await page.getByRole("textbox", { name: "Reason" }).fill("[fail-apply] a broken template")
  await page.getByRole("button", { name: "Submit and commit" }).click()
  await expect(page.getByText("Failed", { exact: true })).toBeVisible({ timeout: 60_000 })
  await expect(page.getByRole("list", { name: "Timeline" })).toContainText("the apply of envs/dev failed")
  await expect(page.getByRole("button", { name: /Retry/ })).not.toBeVisible()

  await page.goto(`/resources/infra-live/vm/dev/${name}`)
  await expect(page.getByText("Failed", { exact: true }).first()).toBeVisible({ timeout: 30_000 })
})
