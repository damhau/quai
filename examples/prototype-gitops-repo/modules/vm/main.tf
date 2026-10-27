# Illustrative only: swap in vsphere_virtual_machine, proxmox_vm_qemu,
# kubevirt_virtual_machine or a cloud provider's instance resource.
resource "terraform_data" "vm" {
  input = {
    name          = "${var.environment}-${var.name}"
    template      = var.template
    cpu           = var.cpu
    memory_mb     = var.memory_gb * 1024
    disks         = var.disks
    vlan          = var.vlan
    ip            = var.ip
    backup_policy = var.backup_policy
    tags          = var.tags
  }
}

output "id" { value = terraform_data.vm.id }
