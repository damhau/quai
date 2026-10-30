# VMs of the booking team. One entry per VM; the platform team reviews changes.
environment = "prod"

vms = {
  # The front-end. Sized for the summer peak.
  web01 = {
    cpu       = 8 # CHG-2291
    memory_gb = 16
    disks     = [{ size_gb = 80, tier = "ssd" }]
  }

  # The database; its data disk is sized by the DBA team.
  db01 = {
    cpu       = 8
    memory_gb = 64
    disks = [
      { size_gb = 100, tier = "ssd" },
      { size_gb = 500, tier = "ssd" }, # data
    ]
  }

  app01 = {
    cpu       = 2
    disks     = [{ size_gb = 40 }]
    memory_gb = 8
  }
}
