locals {
  networkmap = {
    "shared" = {
      fabric = data.maas_vlan.vid-72.fabric
      vlan   = data.maas_vlan.vid-72.id
      subnet = data.maas_subnet.sn-shared-svcs.id
    },
    "storage" = {
      fabric = data.maas_vlan.vid-64.fabric
      vlan   = data.maas_vlan.vid-64.id
      subnet = data.maas_subnet.sn-storage.id
    },
    "eastwest" = {
      fabric = data.maas_vlan.vid-32.fabric
      vlan   = data.maas_vlan.vid-32.id
      subnet = data.maas_subnet.sn-eastwest.id
    }
  }
}
