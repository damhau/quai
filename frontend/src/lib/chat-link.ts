import { useQuery } from "@tanstack/react-query"
import { checkChatLink } from "@/api/generated/sdk.gen"

// Approving from chat (docs/design/05-delivery-and-approvals.md#approving-from-chat): the Approve
// and Deny buttons of an approval card in Microsoft Teams open the request page with a signed link
// (?link=). The page asks the server which decision it opens, for the person signed in, and offers
// to confirm it; the review that confirms it carries the link, which the server checks again with
// every check of a review. Opening the link changes nothing.

/** maxChatLink bounds a link's token, as the server does. */
const maxChatLink = 1024

/** chatLinkOf keeps the link of a page's search when it may be one. */
export function chatLinkOf(v: unknown): string | undefined {
  return typeof v === "string" && v !== "" && v.length <= maxChatLink ? v : undefined
}

/** chatChannels names the chat channels links come from. */
export const chatChannels: Record<string, string> = { teams: "Microsoft Teams", slack: "Slack" }

/**
 * useChatLink asks the server which decision a link opens a request with, once: a refusal is
 * audited, so the answer is kept for the page's life and never asked again on focus or reconnect.
 */
export function useChatLink(id: string, link: string | undefined) {
  return useQuery({
    queryKey: ["chatLink", id, link],
    queryFn: async ({ signal }) => (await checkChatLink({ path: { id }, body: { link: link ?? "" }, signal, throwOnError: true })).data,
    enabled: link !== undefined,
    retry: false,
    staleTime: Infinity,
    gcTime: Infinity,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })
}
