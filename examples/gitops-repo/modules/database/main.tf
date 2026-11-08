# Illustrative only: swap in your database service's instance resource.
resource "terraform_data" "database" {
  input = {
    name                  = "${var.environment}-${var.name}"
    engine                = var.engine
    engine_version        = var.engine_version
    vcpu                  = var.vcpu
    memory_gb             = var.memory_gb
    storage_gb            = var.storage_gb
    backup_retention_days = var.backup_retention_days
  }
}
