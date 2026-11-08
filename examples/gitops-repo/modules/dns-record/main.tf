# Illustrative only: swap in your DNS provider's record set resource.
resource "terraform_data" "record" {
  input = {
    fqdn   = "${var.name}.${var.zone}."
    type   = var.type
    ttl    = var.ttl
    values = var.values
  }
}
