allowed_cidrs = ["10.0.0.0/8", "172.16.0.0/12"]

dns_records = [
  { name = "www", type = "CNAME", value = "booking.example.com" },
  { name = "api", type = "A", value = "10.0.12.10" },
]
