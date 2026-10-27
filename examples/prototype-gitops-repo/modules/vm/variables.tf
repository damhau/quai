variable "name" { type = string }
variable "environment" { type = string }
variable "template" { type = string }
variable "cpu" { type = number }
variable "memory_gb" { type = number }
variable "disks" {
  type = list(object({
    size_gb = number
    tier    = optional(string, "standard")
  }))
}
variable "vlan" { type = number }
variable "ip" {
  type    = string
  default = null
}
variable "backup_policy" {
  type    = string
  default = null
}
variable "tags" {
  type    = map(string)
  default = {}
}
