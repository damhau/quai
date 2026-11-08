# Record sets of the dev.booking.internal zone, by name. The network team reviews every change;
# a CNAME that points outside the internal zones also needs the security team.
records = {
  www      = { owner = "carol", type = "CNAME", values = ["frontend.dev.booking.internal."] }
  frontend = { owner = "carol", type = "A", values = ["10.120.1.10"] }
  payments = { owner = "bob", type = "A", ttl = 60, values = ["10.120.2.10"] }
}
