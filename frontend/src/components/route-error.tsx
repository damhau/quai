import type { ErrorComponentProps } from "@tanstack/react-router"
import { AlertCircle } from "lucide-react"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { problemMessage } from "@/lib/problems"

/** RouteError is what a page shows when its data cannot be loaded. */
export function RouteError({ error }: ErrorComponentProps) {
  return (
    <Alert variant="destructive">
      <AlertCircle />
      <AlertTitle>This page cannot be shown</AlertTitle>
      <AlertDescription>{problemMessage(error)}</AlertDescription>
    </Alert>
  )
}
