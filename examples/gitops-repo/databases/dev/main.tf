# Stack for the databases of the dev environment. Every item of the databases list
# (databases.auto.tfvars.json) becomes one module instance, keyed by its name. Quai only ever
# edits the JSON.
variable "databases" {
  type = list(object({
    name                  = string
    owner                 = string
    engine                = string
    version               = optional(string)
    vcpu                  = number
    memory_gb             = number
    storage_gb            = number
    backup_retention_days = optional(number, 7)
  }))
}

module "database" {
  source   = "../../modules/database"
  for_each = { for db in var.databases : db.name => db }

  environment           = "dev"
  name                  = each.key
  engine                = each.value.engine
  engine_version        = each.value.version
  vcpu                  = each.value.vcpu
  memory_gb             = each.value.memory_gb
  storage_gb            = each.value.storage_gb
  backup_retention_days = each.value.backup_retention_days
}
