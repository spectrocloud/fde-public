data "maas_fabric" "core" {
  name = "fabric-core"
}

data "maas_vlan" "pxe" {
  fabric = data.maas_fabric.core.name
  vlan = 0
}

data "maas_vlan" "vid-32" {
  fabric = data.maas_fabric.core.name
  vlan = 32
}

data "maas_vlan" "vid-64" {
  fabric = data.maas_fabric.core.name
  vlan = 64
}

data "maas_vlan" "vid-72" {
  fabric = data.maas_fabric.core.name
  vlan = 72
}

data "maas_subnet" "sn-shared-svcs" {
  cidr = "10.0.72.0/22"
}

data "maas_subnet" "sn-storage" {
  cidr = "10.0.64.0/21"
}

data "maas_subnet" "sn-eastwest" {
  cidr = "172.16.0.0/16"
}

data "maas_network_interface_physical" "hgx-dpu-p0" {
  for_each = maas_machine.hgx-machines
  machine  = each.value.id
  name     = "ens51f0np0"
}

data "maas_network_interface_physical" "hgx-dpu-p1" {
  for_each = maas_machine.hgx-machines
  machine  = each.value.id
  name    = "ens51f1np1"
}

# data "maas_network_interface_physical" "hgx-ew-p0" {
#   for_each = {for k,v in local.hgx-machines: v.name=>v if v.enabled == true && v.eastwest == true}
#   machine  = maas_machine.hgx-machines[each.key].id
#   name     = "ens4f0np0"
# }
