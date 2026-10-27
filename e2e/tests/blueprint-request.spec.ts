import { expect, test } from "@playwright/test"
import { approve, delivered, preview, signIn, unique } from "./helpers"

// The prototype's first walkthrough: a large VM in production from the linux-vm blueprint needs
// the platform admins and FinOps; once both approve, Quai commits it.
test("a large production VM from a blueprint is approved by platform admins and FinOps, then committed", async ({ page }) => {
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

  await approve(page, "alice", title)
  await expect(page.getByText("Pending approval", { exact: true })).toBeVisible() // FinOps still has to
  await approve(page, "fiona", title)
  await delivered(page)

  await page.getByRole("link", { name: "Git log" }).click()
  await expect(page.getByRole("link", { name: new RegExp(`quai: create linux-vm/${name}`) })).toBeVisible()
})
