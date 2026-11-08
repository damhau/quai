# Illustrative only: swap in kubernetes_namespace and kubernetes_resource_quota. The namespace
# and its quota are two resources, so that a plan gives each change to one Quai type: the
# namespace type, or the namespace-quota type.
resource "terraform_data" "namespace" {
  input = {
    name        = "${var.cluster}/${var.name}"
    owner       = var.owner
    description = var.description
    labels      = var.labels
  }
}

resource "terraform_data" "quota" {
  input = {
    namespace = terraform_data.namespace.output.name
    cpu       = var.quota.cpu
    memory    = "${var.quota.memory_gi}Gi"
    pods      = var.quota.pods
  }
}
