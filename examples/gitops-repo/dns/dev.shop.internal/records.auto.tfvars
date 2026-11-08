# Record sets of the dev.shop.internal zone, by name, one line each.
records = {
  build = { owner = "alice", type = "A", values = ["10.0.11.20"] }
  shop  = { owner = "bob", type = "A", ttl = 60, values = ["10.0.11.30"] }
}
