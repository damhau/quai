import { expect, test } from "@playwright/test"
import { delivered, preview, signIn, unique } from "./helpers"

// The prototype's designer walkthrough, on the new model: a blueprint built from the vm type,
// published to the repository, then requested from the catalog.
test("a blueprint built from the vm type in the designer is published and can be requested", async ({ page }) => {
  const id = unique()
  await signIn(page, "alice", "/designer")
  await page.getByRole("link", { name: "New blueprint" }).click()
  await page.getByRole("button", { name: "Add Virtual machine (Terraform)" }).click()
  const inspector = page.getByRole("group", { name: "Resource vm" })
  await inspector.getByRole("textbox", { name: "Environment" }).fill("dev")
  await inspector.getByRole("textbox", { name: "VM name" }).fill("${{ inputs.name }}")
  await inspector.getByRole("textbox", { name: "Values" }).fill(
    JSON.stringify({ template: "ubuntu-24.04", cpu: 2, memory_gb: 4, disks: [{ size_gb: 50 }], network: { vlan: 110 } }),
  )
  await inspector.getByRole("textbox", { name: "Values" }).blur()

  await page.getByRole("tab", { name: "Form" }).click()
  await page.getByRole("button", { name: "Add a field" }).click()
  await page.getByRole("textbox", { name: "Field 1: name" }).fill("name")
  await page.getByRole("textbox", { name: "Field 1: name" }).blur()
  await page.getByRole("textbox", { name: "Field 1: title" }).fill("VM name")
  await page.getByRole("checkbox", { name: "Field 1: required" }).check()

  await page.getByRole("tab", { name: "About" }).click()
  await page.getByRole("textbox", { name: "Name" }).fill(`e2e-vm-${id}`)
  await page.getByRole("textbox", { name: "Title" }).fill(`E2E VM ${id}`)
  await page.getByRole("textbox", { name: "Category" }).fill("Compute")
  await page.getByRole("button", { name: "Publish" }).click()
  await expect(page).toHaveURL(new RegExp(`/designer/infra-live/e2e-vm-${id}$`))

  await signIn(page, "bob")
  await page.getByRole("link", { name: `Request E2E VM ${id}` }).click()
  await page.getByRole("textbox", { name: /Instance name/ }).fill(`e2e-${id}`)
  await page.getByRole("textbox", { name: /VM name/ }).fill(`e2e${id}`)
  await expect(preview(page).getByText(`envs/dev/vms/e2e${id}.yaml`)).toBeVisible()
  await page.getByRole("button", { name: "Submit and commit" }).click()
  await delivered(page)
})
