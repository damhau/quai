# Stack for the dev environment. Every file in vms/ becomes one module
# instance, keyed by its file name. Quai only ever edits the YAML.
locals {
  vm_files = fileset("${path.module}/vms", "*.yaml")
  vms      = { for f in local.vm_files : trimsuffix(f, ".yaml") => yamldecode(file("${path.module}/vms/${f}")) }
}

module "vm" {
  source   = "../../modules/vm"
  for_each = local.vms

  name          = each.key
  environment   = "dev"
  template      = each.value.template
  cpu           = each.value.cpu
  memory_gb     = each.value.memory_gb
  disks         = each.value.disks
  vlan          = each.value.network.vlan
  ip            = try(each.value.network.ip, null)
  backup_policy = try(each.value.backup, false) ? try(each.value.backup_policy, "daily") : null
  tags          = merge(try(each.value.tags, {}), { owner = try(each.value.owner, "unknown") })
}
