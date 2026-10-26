// The blueprint model. Blueprints are YAML documents stored in the GitOps
// repository under `blueprints/`, so the catalog itself is versioned in Git.

export type InputType = "string" | "integer" | "number" | "boolean" | "enum";

export interface InputDef {
  name: string;
  title?: string;
  description?: string;
  type: InputType;
  required?: boolean;
  default?: unknown;
  /** For `enum` inputs. */
  options?: (string | { value: string; label: string })[];
  min?: number;
  max?: number;
  pattern?: string;
  placeholder?: string;
  /** Expression; when false the field is hidden and its value dropped. */
  visibleIf?: string;
  /** Show the field but prevent changes on day-2 updates. */
  immutable?: boolean;
}

export interface ApprovalRule {
  name: string;
  /** Expression evaluated against { inputs, deployment, request, action }. */
  when?: string;
  /** Principals allowed to approve: `group:<name>` or `user:<name>`. */
  approvers: string[];
  /** Number of distinct approvals required (default 1). */
  min?: number;
}

export interface ApprovalPolicy {
  rules: ApprovalRule[];
}

export interface ResourceNode {
  id: string;
  type: string;
  title?: string;
  /** Component properties; string values may contain `${{ expr }}` bindings. */
  properties?: Record<string, unknown>;
  /** Only render the resource when this expression is true. */
  when?: string;
  /** Designer layout. */
  position?: { x: number; y: number };
}

export interface Connection {
  from: string;
  to: string;
}

export interface ActionDef {
  name: string;
  title?: string;
  description?: string;
  icon?: string;
  inputs?: InputDef[];
  /** Deployment inputs to change; values may reference `action.<input>`. */
  set?: Record<string, unknown>;
  /** Overrides the blueprint approval policy for this action. */
  approval?: ApprovalPolicy;
  /** Expression; hides the action when false (e.g. `inputs.environment != 'prod'`). */
  availableIf?: string;
}

export interface Blueprint {
  apiVersion: "quai.dev/v1alpha1";
  kind: "Blueprint";
  metadata: {
    name: string;
    title: string;
    description?: string;
    icon?: string;
    category?: string;
    owner?: string;
    version?: string;
    tags?: string[];
  };
  spec: {
    inputs: InputDef[];
    resources: ResourceNode[];
    connections?: Connection[];
    approval?: ApprovalPolicy;
    actions?: ActionDef[];
    /** Directory the deployment is written to (default `deployments/${{ deployment.name }}`). */
    path?: string;
  };
}

/** What Quai writes to `quai.yaml` inside every deployment directory. */
export interface DeploymentRecord {
  apiVersion: "quai.dev/v1alpha1";
  kind: "Deployment";
  metadata: { name: string; owner: string; createdAt: string; updatedAt: string };
  spec: { blueprint: string; blueprintVersion: string; inputs: Record<string, unknown> };
}

export interface RenderedFile {
  path: string;
  content: string;
}

export interface FileChange {
  path: string;
  op: "add" | "modify" | "delete";
  before?: string;
  after?: string;
}

export interface Principal {
  name: string;
  displayName?: string;
  email?: string;
  groups: string[];
}
