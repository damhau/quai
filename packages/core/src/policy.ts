import { test, type Scope } from "./expr.ts";
import type { ApprovalPolicy, ApprovalRule, Principal } from "./types.ts";

// Approval model, inspired by Teleport access requests: the requester states a
// reason, each matching rule needs `min` distinct approvals from its eligible
// reviewers, nobody can approve their own request, and one denial rejects it.

export interface Review {
  reviewer: string;
  reviewerGroups: string[];
  decision: "approve" | "deny";
  comment?: string;
  at: string;
}

export interface RuleStatus {
  rule: ApprovalRule;
  approvals: string[];
  satisfied: boolean;
}

export interface PolicyOutcome {
  state: "approved" | "pending" | "denied";
  rules: RuleStatus[];
}

export function matchingRules(policy: ApprovalPolicy | undefined, scope: Scope): ApprovalRule[] {
  return (policy?.rules ?? []).filter((r) => test(r.when, scope));
}

export function canReview(rule: ApprovalRule, who: Pick<Principal, "name" | "groups">): boolean {
  return rule.approvers.some((a) => {
    const [kind, name] = a.includes(":") ? a.split(":", 2) : ["group", a];
    return kind === "user" ? name === who.name : who.groups.includes(name);
  });
}

export function evaluatePolicy(rules: ApprovalRule[], requester: string, reviews: Review[]): PolicyOutcome {
  const valid = reviews.filter((r) => r.reviewer !== requester);
  if (valid.some((r) => r.decision === "deny")) {
    return { state: "denied", rules: rules.map((rule) => ({ rule, approvals: [], satisfied: false })) };
  }
  const statuses = rules.map((rule) => {
    const approvals = [
      ...new Set(
        valid
          .filter((r) => r.decision === "approve" && canReview(rule, { name: r.reviewer, groups: r.reviewerGroups }))
          .map((r) => r.reviewer),
      ),
    ];
    return { rule, approvals, satisfied: approvals.length >= (rule.min ?? 1) };
  });
  return { state: statuses.every((s) => s.satisfied) ? "approved" : "pending", rules: statuses };
}
