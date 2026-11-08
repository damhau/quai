variable "cluster" { type = string }
variable "name" { type = string }
variable "owner" { type = string }
variable "description" {
  type    = string
  default = null
}
variable "labels" {
  type    = map(string)
  default = {}
}
variable "quota" {
  type = object({
    cpu       = number
    memory_gi = number
    pods      = optional(number, 50)
  })
}
