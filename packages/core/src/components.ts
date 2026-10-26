import type { InputDef, ResourceNode } from "./types.ts";

// The component library backs both the designer palette and rendering. Each
// component turns validated properties into Kubernetes manifests that a GitOps
// controller (Argo CD / Flux) reconciles: Crossplane claims for cloud
// services, KubeVirt for VMs, tf-controller for plain Terraform modules.

export type Manifest = Record<string, unknown>;

export interface RenderContext {
  node: ResourceNode;
  props: Record<string, any>;
  deployment: { name: string; owner: string };
  namespace: string;
  labels: Record<string, string>;
  /** Resources this node is wired to on the canvas (node -> target). */
  targets: { node: ResourceNode; component: ComponentType }[];
}

export interface ComponentType {
  type: string;
  title: string;
  description: string;
  category: "Compute" | "Data" | "Network" | "Delivery" | "Infrastructure" | "Other";
  icon: string;
  color: string;
  properties: InputDef[];
  /** Name of the secret this component publishes for consumers, if any. */
  connectionSecret?: (node: ResourceNode) => string;
  render(ctx: RenderContext): Manifest[];
}

const meta = (ctx: RenderContext, name = `${ctx.deployment.name}-${ctx.node.id}`, namespaced = true) => ({
  name,
  ...(namespaced ? { namespace: ctx.namespace } : {}),
  labels: { ...ctx.labels, "quai.dev/resource": ctx.node.id },
});

const dependsOn = (ctx: RenderContext) =>
  ctx.targets.length ? { "quai.dev/depends-on": ctx.targets.map((t) => t.node.id).join(",") } : undefined;

const connSecret = (node: ResourceNode) => `${node.id}-connection`;

const OS_IMAGES: Record<string, string> = {
  "fedora-40": "quay.io/containerdisks/fedora:40",
  "ubuntu-24.04": "quay.io/containerdisks/ubuntu:24.04",
  "centos-stream-9": "quay.io/containerdisks/centos-stream:9",
  "rhel-9": "registry.redhat.io/rhel9/rhel-guest-image:latest",
};

export const components: ComponentType[] = [
  {
    type: "kubevirt.vm",
    title: "Virtual machine",
    description: "A KubeVirt VirtualMachine with a root disk cloned from an OS image.",
    category: "Compute",
    icon: "server",
    color: "#6366f1",
    properties: [
      { name: "os", title: "Operating system", type: "enum", options: Object.keys(OS_IMAGES), default: "fedora-40" },
      { name: "cpu", title: "vCPUs", type: "integer", min: 1, max: 64, default: 2 },
      { name: "memoryGi", title: "Memory (GiB)", type: "integer", min: 1, max: 512, default: 4 },
      { name: "diskGi", title: "Disk (GiB)", type: "integer", min: 10, max: 4096, default: 30 },
      { name: "running", title: "Running", type: "boolean", default: true },
      { name: "sshKey", title: "SSH public key", type: "string" },
    ],
    render(ctx) {
      const p = ctx.props;
      const name = `${ctx.deployment.name}-${ctx.node.id}`;
      const vm: Manifest = {
        apiVersion: "kubevirt.io/v1",
        kind: "VirtualMachine",
        metadata: { ...meta(ctx), annotations: dependsOn(ctx) },
        spec: {
          runStrategy: p.running ? "Always" : "Halted",
          dataVolumeTemplates: [
            {
              metadata: { name: `${name}-root` },
              spec: {
                source: { registry: { url: `docker://${OS_IMAGES[p.os]}` } },
                storage: { resources: { requests: { storage: `${p.diskGi}Gi` } } },
              },
            },
          ],
          template: {
            metadata: { labels: { "kubevirt.io/domain": name } },
            spec: {
              domain: {
                cpu: { cores: p.cpu },
                memory: { guest: `${p.memoryGi}Gi` },
                devices: { disks: [{ name: "root", disk: { bus: "virtio" } }] },
              },
              volumes: [
                { name: "root", dataVolume: { name: `${name}-root` } },
                ...(p.sshKey
                  ? [{ name: "cloudinit", cloudInitNoCloud: { userData: `#cloud-config\nssh_authorized_keys:\n  - ${p.sshKey}\n` } }]
                  : []),
              ],
            },
          },
        },
      };
      if (!dependsOn(ctx)) delete (vm.metadata as any).annotations;
      return [vm];
    },
  },
  {
    type: "crossplane.postgres",
    title: "PostgreSQL",
    description: "Managed PostgreSQL via a Crossplane composite resource claim.",
    category: "Data",
    icon: "database",
    color: "#0ea5e9",
    connectionSecret: connSecret,
    properties: [
      { name: "version", title: "Version", type: "enum", options: ["14", "15", "16", "17"], default: "16" },
      { name: "size", title: "Instance size", type: "enum", options: ["small", "medium", "large", "xlarge"], default: "small" },
      { name: "storageGB", title: "Storage (GB)", type: "integer", min: 10, max: 16384, default: 20 },
      { name: "highAvailability", title: "High availability", type: "boolean", default: false },
      { name: "backupRetentionDays", title: "Backup retention (days)", type: "integer", min: 0, max: 35, default: 7 },
    ],
    render(ctx) {
      const p = ctx.props;
      return [
        {
          apiVersion: "database.platform.quai.dev/v1alpha1",
          kind: "PostgreSQLInstance",
          metadata: meta(ctx),
          spec: {
            parameters: {
              version: String(p.version),
              size: p.size,
              storageGB: p.storageGB,
              highAvailability: p.highAvailability,
              backupRetentionDays: p.backupRetentionDays,
            },
            compositionSelector: { matchLabels: { provider: "default" } },
            writeConnectionSecretToRef: { name: connSecret(ctx.node) },
          },
        },
      ];
    },
  },
  {
    type: "crossplane.bucket",
    title: "Object storage",
    description: "An S3-compatible bucket via a Crossplane claim.",
    category: "Data",
    icon: "bucket",
    color: "#14b8a6",
    connectionSecret: connSecret,
    properties: [
      { name: "region", title: "Region", type: "string", default: "eu-central-1" },
      { name: "versioning", title: "Versioning", type: "boolean", default: true },
      { name: "public", title: "Public read access", type: "boolean", default: false },
    ],
    render(ctx) {
      const p = ctx.props;
      return [
        {
          apiVersion: "storage.platform.quai.dev/v1alpha1",
          kind: "ObjectBucket",
          metadata: meta(ctx),
          spec: {
            parameters: { region: p.region, versioning: p.versioning, publicRead: p.public },
            writeConnectionSecretToRef: { name: connSecret(ctx.node) },
          },
        },
      ];
    },
  },
  {
    type: "k8s.namespace",
    title: "Namespace",
    description: "A Kubernetes namespace with a resource quota.",
    category: "Infrastructure",
    icon: "box",
    color: "#64748b",
    properties: [
      { name: "cpuQuota", title: "CPU quota", type: "string", default: "8" },
      { name: "memoryQuota", title: "Memory quota", type: "string", default: "16Gi" },
    ],
    render(ctx) {
      return [
        { apiVersion: "v1", kind: "Namespace", metadata: meta(ctx, ctx.namespace, false) },
        {
          apiVersion: "v1",
          kind: "ResourceQuota",
          metadata: meta(ctx, "quota"),
          spec: { hard: { "requests.cpu": ctx.props.cpuQuota, "requests.memory": ctx.props.memoryQuota } },
        },
      ];
    },
  },
  {
    type: "argocd.app",
    title: "Application",
    description: "An Argo CD Application deploying a Helm chart. Wired data services are passed as Helm values.",
    category: "Delivery",
    icon: "rocket",
    color: "#f97316",
    properties: [
      { name: "repoURL", title: "Chart repository", type: "string", required: true, default: "https://charts.example.com" },
      { name: "chart", title: "Chart", type: "string", required: true, default: "web-app" },
      { name: "version", title: "Chart version", type: "string", default: "1.0.0" },
      { name: "replicas", title: "Replicas", type: "integer", min: 0, max: 50, default: 2 },
      { name: "argoNamespace", title: "Argo CD namespace", type: "string", default: "argocd" },
    ],
    render(ctx) {
      const p = ctx.props;
      const params = [
        { name: "replicaCount", value: String(p.replicas) },
        ...ctx.targets
          .filter((t) => t.component.connectionSecret)
          .map((t) => ({ name: `connections.${t.node.id}.secretName`, value: t.component.connectionSecret!(t.node) })),
      ];
      return [
        {
          apiVersion: "argoproj.io/v1alpha1",
          kind: "Application",
          metadata: { ...meta(ctx), namespace: p.argoNamespace },
          spec: {
            project: "default",
            source: { repoURL: p.repoURL, chart: p.chart, targetRevision: p.version, helm: { parameters: params } },
            destination: { server: "https://kubernetes.default.svc", namespace: ctx.namespace },
            syncPolicy: { automated: { prune: true, selfHeal: true }, syncOptions: ["CreateNamespace=true"] },
          },
        },
      ];
    },
  },
  {
    type: "terraform.module",
    title: "Terraform module",
    description: "Runs a Terraform/OpenTofu module through the Flux tf-controller. Outputs land in a secret.",
    category: "Infrastructure",
    icon: "terraform",
    color: "#7c3aed",
    connectionSecret: (node) => `${node.id}-outputs`,
    properties: [
      { name: "source", title: "Flux GitRepository", type: "string", required: true, default: "terraform-modules" },
      { name: "path", title: "Module path", type: "string", required: true, default: "./modules/network" },
      { name: "autoApprove", title: "Auto-apply plan", type: "boolean", default: true },
    ],
    render(ctx) {
      const p = ctx.props;
      const vars = Object.entries((p.vars as Record<string, unknown>) ?? {}).map(([name, value]) => ({ name, value }));
      return [
        {
          apiVersion: "infra.contrib.fluxcd.io/v1alpha2",
          kind: "Terraform",
          metadata: meta(ctx),
          spec: {
            interval: "10m",
            approvePlan: p.autoApprove ? "auto" : "",
            path: p.path,
            sourceRef: { kind: "GitRepository", name: p.source, namespace: "flux-system" },
            vars,
            writeOutputsToSecret: { name: `${ctx.node.id}-outputs` },
          },
        },
      ];
    },
  },
  {
    type: "raw",
    title: "Raw manifest",
    description: "Any Kubernetes manifest, for when no component fits. Bindings work in every field.",
    category: "Other",
    icon: "code",
    color: "#94a3b8",
    properties: [],
    render(ctx) {
      const m = (ctx.props.manifest ?? {}) as Manifest;
      const md = (m.metadata ?? {}) as Record<string, any>;
      return [{ ...m, metadata: { namespace: ctx.namespace, ...md, labels: { ...ctx.labels, ...(md.labels ?? {}) } } }];
    },
  },
];

const byType = new Map(components.map((c) => [c.type, c]));

export function getComponent(type: string): ComponentType | undefined {
  return byType.get(type);
}

/** Properties that are passed through without validation (free-form objects). */
export const FREEFORM_PROPS: Record<string, string[]> = { raw: ["manifest"], "terraform.module": ["vars"] };
