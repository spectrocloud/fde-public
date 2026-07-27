locals {
  hgx-machines = [
    {
      name       = "hgx-r1-01"
      pool       = "hgx-8-gpu-shared"
      enabled    = true
      zone       = "default"
      power_addr = "10.0.50.10"
      power_user = "ADMIN"
      power_pass = "example"
      pxe_mac    = "aa:bb:cc:dd:ee:ff"
      rack       = 1
      shared     = true
      storage    = true
      customer   = ""
      ip_mode_sh = "AUTO"
      ip_addr_sh = ""
      ip_mode_st = "AUTO"
      ip_addr_st = ""
      ip_mode_dd = "AUTO"
      ip_addr_dd = ""
    },
    {
      name       = "hgx-r1-02"
      pool       = "hgx-8-gpu-shared"
      enabled    = true
      zone       = "default"
      power_addr = "10.0.50.11"
      power_user = "ADMIN"
      power_pass = "example"
      pxe_mac    = "11:22:33:44:55:66"
      rack       = 1
      shared     = true
      storage    = true
      customer   = ""
      ip_mode_sh = "AUTO"
      ip_addr_sh = ""
      ip_mode_st = "AUTO"
      ip_addr_st = ""
      ip_mode_dd = "AUTO"
      ip_addr_dd = ""
    }
  ]
}
