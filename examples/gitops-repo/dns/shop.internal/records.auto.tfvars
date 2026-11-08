# Record sets of the shop.internal zone, by name. The platform team reviews every change.
records = {
  www = {
    owner  = "bob"
    type   = "CNAME"
    values = ["web01.shop.internal."]
  }
  web01 = {
    owner  = "bob"
    type   = "A"
    values = ["10.0.12.11"]
  }
  # Clients cache the database's address for a day: it moves only with a migration.
  shop-db = {
    owner  = "bob"
    type   = "A"
    ttl    = 24 * 3600
    values = ["10.0.13.30"]
  }
}
