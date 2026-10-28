import type { ReactNode } from "react"
import { Link, useNavigate, useRouterState } from "@tanstack/react-router"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { Boxes, ClipboardCheck, FolderGit2, GitCommitHorizontal, LayoutGrid, ListChecks, LogOut, Workflow } from "lucide-react"
import { getMeOptions, listRequestsOptions } from "@/api/generated/@tanstack/react-query.gen"
import { Badge } from "@/components/ui/badge"
import { logout } from "@/lib/auth"
import { isAdmin } from "@/lib/repositories"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"

const nav = [
  { to: "/", label: "Catalog", icon: LayoutGrid },
  { to: "/inventory", label: "Inventory", icon: Boxes },
  { to: "/requests", label: "Requests", icon: ListChecks },
  { to: "/approvals", label: "Approvals", icon: ClipboardCheck },
  { to: "/designer", label: "Designer", icon: Workflow },
  { to: "/git", label: "Git log", icon: GitCommitHorizontal },
] as const

// For repository and platform admins.
const admin = [{ to: "/admin/repositories", label: "Repositories", icon: FolderGit2 }] as const

export function AppShell({ children }: { children: ReactNode }) {
  const path = useRouterState({ select: (s) => s.location.pathname })
  const { data: me } = useQuery({ ...getMeOptions(), enabled: path !== "/login" })
  if (path === "/login") return <main className="min-h-svh">{children}</main>
  const links = isAdmin(me) ? [...nav, ...admin] : nav
  return (
    <div className="flex min-h-svh flex-col">
      <header className="border-b">
        <div className="mx-auto flex h-14 max-w-7xl items-center gap-6 px-4">
          <Link to="/" className="font-semibold tracking-tight">
            Quai
          </Link>
          <nav aria-label="Main" className="flex items-center gap-1">
            {links.map(({ to, label, icon: Icon }) => (
              <Link
                key={to}
                to={to}
                activeOptions={{ exact: to === "/" }}
                className="text-muted-foreground hover:text-foreground flex items-center gap-1.5 rounded-md px-2.5 py-1.5 text-sm"
                activeProps={{ className: "bg-accent text-foreground" }}
              >
                <Icon className="size-4" aria-hidden />
                {label}
                {to === "/approvals" && <InboxCount />}
              </Link>
            ))}
          </nav>
          <div className="ml-auto">
            <UserMenu />
          </div>
        </div>
      </header>
      <main className="mx-auto w-full max-w-7xl flex-1 px-4 py-6">{children}</main>
    </div>
  )
}

/** InboxCount shows how many requests wait for the signed-in person's review. */
function InboxCount() {
  const { data } = useQuery({ ...listRequestsOptions({ query: { view: "inbox" } }), refetchInterval: 30_000 })
  const n = data?.length ?? 0
  if (!n) return null
  return (
    <Badge className="h-5 min-w-5 rounded-full px-1.5" aria-label={`${n} waiting`}>
      {n}
    </Badge>
  )
}

function UserMenu() {
  const { data: me } = useQuery(getMeOptions())
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  if (!me) return null
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="sm" aria-label="Account">
          {me.name}
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-56">
        <DropdownMenuLabel className="font-normal">
          <div className="font-medium">{me.name}</div>
          {me.email && <div className="text-muted-foreground text-xs">{me.email}</div>}
        </DropdownMenuLabel>
        <DropdownMenuSeparator />
        {(me.roles ?? []).map((r) => (
          <DropdownMenuLabel key={`${r.role}@${r.scope}`} className="text-muted-foreground text-xs font-normal">
            {r.role} · {r.scope}
          </DropdownMenuLabel>
        ))}
        <DropdownMenuSeparator />
        <DropdownMenuItem
          onSelect={async () => {
            await logout(queryClient)
            await navigate({ to: "/login", search: { next: "/" } })
          }}
        >
          <LogOut className="size-4" aria-hidden />
          Sign out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
