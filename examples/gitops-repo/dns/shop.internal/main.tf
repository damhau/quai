# Stack for the shop.internal zone. Every entry of the records map (records.auto.tfvars) becomes
# one record set, keyed by its name. Quai only ever edits the tfvars.
variable "records" {
  type = map(object({
    owner  = string
    type   = string
    ttl    = optional(number, 300)
    values = list(string)
  }))
}

module "record" {
  source   = "../../modules/dns-record"
  for_each = var.records

  zone   = "shop.internal"
  name   = each.key
  type   = each.value.type
  ttl    = each.value.ttl
  values = each.value.values
}
