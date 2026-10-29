variable "name" { type = string }
variable "admin_password" {
  type      = string
  sensitive = true
}

resource "terraform_data" "vm" {
  input = {
    name     = var.name
    cpu      = 2
    password = var.admin_password
  }
}
