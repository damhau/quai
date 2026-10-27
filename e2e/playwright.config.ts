import { defineConfig, devices } from "@playwright/test"

// End-to-end scenarios against the compose stack (docker-compose.yml, profile e2e): the server, a
// worker, PostgreSQL and Gitea seeded with examples/gitops-repo. They share the stack's state, so
// they run one at a time, and name what they create uniquely so that they can run again.
const output = process.env.E2E_OUTPUT ?? "."

export default defineConfig({
  testDir: "tests",
  workers: 1,
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  timeout: 120_000,
  expect: { timeout: 15_000 },
  outputDir: `${output}/test-results`,
  reporter: [["list"], ["html", { open: "never", outputFolder: `${output}/playwright-report` }]],
  use: {
    baseURL: process.env.E2E_BASE_URL ?? "http://localhost:8080",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
})
