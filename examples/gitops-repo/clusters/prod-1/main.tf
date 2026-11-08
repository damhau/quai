# Stack for the namespaces of the prod-1 cluster. Every entry of namespaces.yaml becomes one
# module instance, keyed by its name. Quai only ever edits the YAML.
locals {
  namespaces = { for ns in yamldecode(file("${path.module}/namespaces.yaml")).namespaces : ns.name => ns }
}

module "namespace" {
  source   = "../../modules/namespace"
  for_each = local.namespaces

  cluster     = "prod-1"
  name        = each.key
  owner       = each.value.owner
  description = try(each.value.description, null)
  labels      = try(each.value.labels, {})
  quota       = each.value.quota
}
