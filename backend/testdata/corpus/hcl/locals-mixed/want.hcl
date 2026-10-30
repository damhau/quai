variable "environment" {
  type = string
}

locals {
  # Naming: <team>-<environment>-<role>.
  team   = "booking"
  prefix = "${local.team}-${var.environment}"

  sizes = {
    small  = { cpu = 2, memory_gb = 4 }
    large  = { cpu = 8, memory_gb = 64 }
    medium = { cpu = 4, memory_gb = 16 }
  }
  default_size = var.environment == "prod" ? "large" : "small"
}
