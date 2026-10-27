import { useEffect } from "react"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { getMetaOptions } from "@/api/generated/@tanstack/react-query.gen"
import { devLogin, listDevUsers, localPath, oidcLogin } from "@/lib/auth"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"

export const Route = createFileRoute("/login")({
  validateSearch: (search: Record<string, unknown>) => ({ next: localPath(search.next) }),
  component: Login,
})

function Login() {
  const { next } = Route.useSearch()
  const { data: meta, error } = useQuery(getMetaOptions())
  useEffect(() => {
    if (meta?.auth === "oidc") oidcLogin(next)
  }, [meta?.auth, next])
  return (
    <div className="flex min-h-svh items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle>Sign in to Quai</CardTitle>
          <CardDescription>
            {meta?.auth === "dev" && "Development mode: pick a user."}
            {meta?.auth === "oidc" && "Redirecting to your organization's sign-in…"}
            {meta?.auth === "none" && "This server has no database, so nobody can sign in."}
            {error && "The server cannot be reached."}
          </CardDescription>
        </CardHeader>
        {meta?.auth === "dev" && (
          <CardContent>
            <DevUsers next={next} />
          </CardContent>
        )}
      </Card>
    </div>
  )
}

function DevUsers({ next }: { next: string }) {
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const { data: users = [] } = useQuery({ queryKey: ["dev-users"], queryFn: listDevUsers })
  const signIn = useMutation({
    mutationFn: devLogin,
    onSuccess: async () => {
      queryClient.clear()
      await navigate({ to: next })
    },
  })
  return (
    <ul className="grid gap-2">
      {users.map((u) => (
        <li key={u.Name}>
          <Button variant="outline" className="w-full justify-between" onClick={() => signIn.mutate(u.Name)} disabled={signIn.isPending}>
            <span>{u.Name}</span>
            <span className="text-muted-foreground text-xs">{(u.Groups ?? []).join(", ")}</span>
          </Button>
        </li>
      ))}
    </ul>
  )
}
