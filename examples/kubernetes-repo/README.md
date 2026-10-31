# Kubernetes example repository

A GitOps repository for a Kubernetes platform, reconciled by Argo CD or Flux, with Quai
definitions added: a type for each built-in component of the `component` generator
(docs/design/02-resource-types-and-generators.md). The backend's tests lint it, run its golden
examples, and check its manifests against a Kubernetes API server in server-side dry runs.

Each resource is a directory (the `dir` locator) that holds:

- `quai.yaml`, the record of the form's values, which Quai reads back;
- `manifests.yaml`, the manifests the component renders, one YAML document each;
- `kustomization.yaml`, which lists the manifests (not the record) for Argo CD, Flux or
  `kubectl apply -k`.

The types:

| Type | Component | Directory | Renders |
| --- | --- | --- | --- |
| `namespace` | `k8s.namespace` | `namespaces/<name>/` | a team's `Namespace` and its `ResourceQuota` |
| `config` | `raw` | `configs/<namespace>/<name>/` | a `ConfigMap` |
| `database` | `crossplane.claim` | `databases/<namespace>/<name>/` | a `PostgreSQLInstance` claim of the XRD in `platform/xrds/` |
| `app` | `argocd.application` | `apps/<name>/` | an Argo CD `Application` deploying a path of a team's repository |
| `sync` | `flux.kustomization` | `flux/<name>/` | a Flux `Kustomization` applying a path of the `platform` source |

`schemas/` holds the forms' JSON Schemas and `.quai/tests/<type>/` the golden examples.
