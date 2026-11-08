variable "environment" { type = string }
variable "name" { type = string }
variable "engine" { type = string }
variable "engine_version" {
  type    = string
  default = null # the service's default
}
variable "vcpu" { type = number }
variable "memory_gb" { type = number }
variable "storage_gb" { type = number }
variable "backup_retention_days" {
  type    = number
  default = 7
}
