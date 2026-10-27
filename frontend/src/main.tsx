import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { QueryClientProvider } from "@tanstack/react-query"
import { RouterProvider } from "@tanstack/react-router"
import { configureClient } from "@/api/client"
import { queryClient } from "@/lib/query-client"
import { createAppRouter } from "@/router"
import { Toaster } from "@/components/ui/sonner"
import "./index.css"

configureClient(queryClient)
const router = createAppRouter(queryClient)

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
      <Toaster />
    </QueryClientProvider>
  </StrictMode>,
)
