variable "name" { type = string }
variable "vms" { type = list(string) }
variable "admin_password" {
  type      = string
  sensitive = true
}

module "vm" {
  source   = "../vm"
  for_each = toset(var.vms)

  name           = "${var.name}-${each.key}"
  admin_password = var.admin_password
}
