variable "zone" { type = string }
variable "name" { type = string }
variable "type" { type = string }
variable "ttl" {
  type    = number
  default = 300
}
variable "values" { type = list(string) }
