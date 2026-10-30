allowed_cidrs = ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"]

dns_records = [
  { name = "www", type = "CNAME", value = "booking.example.com" },
  { name = "mail", type = "MX", value = "mx.example.com" },
]
