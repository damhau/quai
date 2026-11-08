# Record sets of the booking.internal zone, by name. The network team reviews every change; a
# CNAME that points outside the internal zones also needs the security team.
records = {
  www = {
    owner  = "carol"
    type   = "CNAME"
    ttl    = 300
    values = ["frontend.booking.internal."]
  }
  frontend = {
    owner  = "carol"
    type   = "A"
    ttl    = 300
    values = ["10.20.1.10", "10.20.1.11"]
  }
  api = {
    owner  = "carol"
    type   = "A"
    ttl    = 60
    values = ["10.20.1.20"]
  }
  payments = {
    owner  = "bob"
    type   = "A"
    values = ["10.20.2.10"]
  }
}
