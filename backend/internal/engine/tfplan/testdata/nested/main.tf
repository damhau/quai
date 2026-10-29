# A root module for the plan fixtures: applications whose VMs are a module inside a module, a
# sensitive input, and a resource no Quai type claims. Only the built-in terraform_data
# resource, so planning needs no provider and no network.
variable "admin_password" {
  type      = string
  sensitive = true
  default   = "fixture-secret-value"
}

module "app" {
  source   = "./modules/app"
  for_each = { shop = ["db", "web"] }

  name           = each.key
  vms            = each.value
  admin_password = var.admin_password
}

resource "terraform_data" "unclaimed" {
  input = "not described by any Quai type"
}
