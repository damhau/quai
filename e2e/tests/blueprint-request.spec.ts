import { expect, test } from "@playwright/test"
import { approvalOnGitea, approve, delivered, onGitea, preview, pullRequest, signIn, unique } from "./helpers"

// The prototype's first walkthrough: a large VM in production from the linux-vm blueprint needs
// the platform admins and FinOps. Its pull request waits on quai/approval; the last approval
// turns it to success, and Quai merges it.
test("a large production VM from a blueprint is approved by platform admins and FinOps, then merged through its pull request", async ({ page }) => {
  const name = `web${unique()}`
  await signIn(page, "bob")
  await page.getByRole("link", { name: "Request Linux virtual machine" }).click()
  await page.getByRole("textbox", { name: /Instance name/ }).fill(name)
  await page.getByRole("radio", { name: "prod" }).click()
  await page.getByRole("textbox", { name: /VM name/ }).fill(name)
  await page.getByRole("radio", { name: "large" }).click()

  await expect(preview(page).getByText(`envs/prod/vms/${name}.yaml`)).toBeVisible()
  await expect(preview(page).getByText(`.quai/instances/linux-vm/${name}.yaml`)).toBeVisible()
  await expect(preview(page).getByText("group:platform-admins")).toBeVisible()
  await expect(preview(page).getByText("group:finops").first()).toBeVisible()
  await page.getByRole("textbox", { name: "Reason" }).fill("Capacity for the launch")
  await page.getByRole("button", { name: "Submit for approval" }).click()
  const title = `Create linux-vm/${name}`
  await expect(page.getByRole("heading", { name: new RegExp(title) })).toBeVisible()
  await expect(page.getByText("Pending approval", { exact: true })).toBeVisible()
  const pr = await pullRequest(page)
  await approvalOnGitea(pr, "pending")

  await approve(page, "alice", title)
  await expect(page.getByText("Pending approval", { exact: true })).toBeVisible() // FinOps still has to
  await approvalOnGitea(pr, "pending")
  await approve(page, "fiona", title)
  await delivered(page)
  expect(await onGitea(pr)).toEqual({ merged: true, approval: "success" })

  await page.getByRole("link", { name: "Git log" }).click()
  await expect(page.getByRole("link", { name: new RegExp(`quai: create linux-vm/${name}`) })).toBeVisible()
})
