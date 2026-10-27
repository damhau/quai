import { Outlet, createRootRouteWithContext, redirect } from "@tanstack/react-router"
import type { QueryClient } from "@tanstack/react-query"
import { getMeOptions, getMetaOptions } from "@/api/generated/@tanstack/react-query.gen"
import { isUnauthorized } from "@/lib/auth"
import { AppShell } from "@/components/app-shell"

export const Route = createRootRouteWithContext<{ queryClient: QueryClient }>()({
  // Everything but the login page needs someone signed in: without a session, go to the login
  // page, which knows how this server signs people in.
  beforeLoad: async ({ context, location }) => {
    if (location.pathname === "/login") return
    await context.queryClient.ensureQueryData(getMetaOptions())
    try {
      await context.queryClient.ensureQueryData(getMeOptions())
    } catch (error) {
      if (!isUnauthorized(error)) throw error
      throw redirect({ to: "/login", search: { next: location.href } })
    }
  },
  component: Root,
})

function Root() {
  return (
    <AppShell>
      <Outlet />
    </AppShell>
  )
}
