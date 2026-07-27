resource "maas_machine" "hgx-machines" {
  for_each           = {for k,v in local.hgx-machines: v.name=>v if v.enabled == true}
  architecture       = "amd64/generic"
  domain             = "company.com"
  hostname           = each.value.name
  pool               = each.value.pool
  power_parameters   = jsonencode({
    "power_address"   = each.value.power_addr
    "power_user"      = each.value.power_user
    "power_pass"      = each.value.power_pass
    "node_id"         = ""
  })
  power_type         = "redfish"
  # To use IPMI instead of Redfish:
  # power_parameters   = jsonencode({
  #   "power_driver"    = "LAN_2_0"
  #   "power_address"   = each.value.power_addr
  #   "power_off_mode"  = "hard"
  #   "cipher_suite_id" = "3"
  #   "power_boot_type" = "efi"
  #   "privilege_level" = "ADMIN"
  #   "power_user"      = each.value.power_user
  #   "power_pass"      = each.value.power_pass
  # })
  # power_type         = "ipmi"
  pxe_mac_address    = lower(each.value.pxe_mac)
  zone               = each.value.zone

  timeouts {
    create = "10m"
  }
}

resource "maas_block_device" "hgx-raid-disk-1" {
  for_each       = maas_machine.hgx-machines
  machine        = each.value.id
  name           = [for disk in each.value.block_devices: disk if disk.size_gigabytes <= 1000].0.name
  id_path        = [for disk in each.value.block_devices: disk if disk.size_gigabytes <= 1000].0.id_path
  is_boot_device = true
  block_size     = 4096
  size_gigabytes = 960
  tags = [
    "ssd",
  ]

  partitions {
    size_gigabytes = 1
    fs_type        = "fat32"
    label          = "efi"
    mount_point    = "/boot/efi"
  }

  partitions {
    size_gigabytes = 958
  }
}

resource "maas_block_device" "hgx-raid-disk-2" {
  for_each       = maas_machine.hgx-machines
  machine        = each.value.id
  name           = [for disk in each.value.block_devices: disk if disk.size_gigabytes <= 1000].1.name
  id_path        = [for disk in each.value.block_devices: disk if disk.size_gigabytes <= 1000].1.id_path
  block_size     = 4096
  size_gigabytes = 960
  tags = [
    "ssd",
  ]

  partitions {
    size_gigabytes = 1
    fs_type        = "fat32"
    label          = "efi"
  }

  partitions {
    size_gigabytes = 958
  }
}

resource "maas_raid" "hgx-boot-raid1" {
  for_each    = maas_machine.hgx-machines
  machine     = each.value.id
  fs_type     = "ext4"
  mount_point = "/"
  name        = "md0"
  level       = "1"

  partitions = [
    maas_block_device.hgx-raid-disk-1[each.key].partitions.1.id,
    maas_block_device.hgx-raid-disk-2[each.key].partitions.1.id,
  ]
}

# To allow managing East-West NICs at the OS level, uncomment the 2 blocks below and repeat for each E-W NIC
# Typically not enabled, we prep East-West NICs via Spectro nodeprep and handle IP addressing through NV-IPAM.
#
# resource "maas_network_interface_physical" "hgx-ew-p0" {
#   for_each    = {for k,v in local.hgx-machines: v.name=>v if v.enabled == true && v.eastwest == true}
#   machine     = try(maas_machine.hgx-machines[each.key].id, null)
#   mac_address = data.maas_network_interface_physical.hgx-ew-p0[each.key].mac_address
#   name        = data.maas_network_interface_physical.hgx-ew-p0[each.key].name
#   vlan        = data.maas_vlan.vid-32.id
#   tags        = ["sriov","tf-managed"]
#   lifecycle {
#     ignore_changes = [
#       mac_address
#     ]
#   }
# }
#
# resource "maas_network_interface_link" "hgx-ew-p0-link" {
#   for_each          = {for k,v in local.hgx-machines: v.name=>v if v.enabled == true && v.eastwest == true}
#   machine           = maas_machine.hgx-machines[each.key].id
#   network_interface = maas_network_interface_physical.hgx-ew-p0[each.key].id
#   subnet            = local.networkmap["eastwest"].subnet
#   mode              = "AUTO"
# }

resource "maas_network_interface_physical" "hgx-dpu-p0" {
  for_each    = maas_machine.hgx-machines
  machine     = each.value.id
  mac_address = data.maas_network_interface_physical.hgx-dpu-p0[each.key].mac_address
  name        = data.maas_network_interface_physical.hgx-dpu-p0[each.key].name
  vlan        = data.maas_vlan.pxe.id
  tags        = ["sriov","tf-managed"]
  lifecycle {
    ignore_changes = [
      mac_address
    ]
  }
}

resource "maas_network_interface_physical" "hgx-dpu-p1" {
  for_each    = maas_machine.hgx-machines
  machine     = each.value.id
  mac_address = data.maas_network_interface_physical.hgx-dpu-p1[each.key].mac_address
  name        = data.maas_network_interface_physical.hgx-dpu-p1[each.key].name
  vlan        = data.maas_vlan.pxe.id
  tags        = ["sriov","tf-managed"]
  lifecycle {
    ignore_changes = [
      mac_address
    ]
  }
}

# IF DPU has Compute Engine enabled, don't bond at the OS level as this will not work.
resource "maas_network_interface_bond" "hgx-dpu-bond0" {
  for_each              = maas_machine.hgx-machines
  machine               = each.value.id
  name                  = "bond0"
  accept_ra             = false
  bond_lacp_rate        = "fast"
  bond_downdelay        = 200
  bond_updelay          = 200
  bond_xmit_hash_policy = "layer3+4"
  bond_miimon           = 100
  bond_mode             = "802.3ad"
  mtu                   = 9216
  parents               = ["ens51f0np0", "ens51f1np1"]
  depends_on            = [
    maas_network_interface_physical.hgx-dpu-p0,
    maas_network_interface_physical.hgx-dpu-p1,
  ]
}

resource "maas_network_interface_vlan" "hgx-dpu-bond0-vlan-shared" {
  for_each  = {for k,v in local.hgx-machines: v.name=>v if v.enabled == true && v.shared == true}
  machine   = maas_machine.hgx-machines[each.key].id
  parent    = maas_network_interface_bond.hgx-dpu-bond0[each.key].name
  vlan      = local.networkmap["shared"].vlan
  fabric    = local.networkmap["shared"].fabric
  accept_ra = false
}

resource "maas_network_interface_vlan" "hgx-dpu-bond0-vlan-storage" {
  for_each  = {for k,v in local.hgx-machines: v.name=>v if v.enabled == true && v.storage == true}
  machine   = maas_machine.hgx-machines[each.key].id
  parent    = maas_network_interface_bond.hgx-dpu-bond0[each.key].name
  vlan      = local.networkmap["storage"].vlan
  fabric    = local.networkmap["storage"].fabric
  accept_ra = false
}

resource "maas_network_interface_vlan" "hgx-dpu-bond0-vlan-dedicated" {
  for_each  = {for k,v in local.hgx-machines: v.name=>v if v.enabled == true && v.customer != ""}
  machine   = maas_machine.hgx-machines[each.key].id
  parent    = maas_network_interface_bond.hgx-dpu-bond0[each.key].name
  vlan      = local.networkmap["${each.value.customer}"].vlan
  fabric    = local.networkmap["${each.value.customer}"].fabric
  accept_ra = false
}

resource "maas_network_interface_link" "hgx-dpu-bond0-vlan-shared-link" {
  for_each          = {for k,v in local.hgx-machines: v.name=>v if v.enabled == true && v.shared == true}
  machine           = maas_machine.hgx-machines[each.key].id
  network_interface = maas_network_interface_vlan.hgx-dpu-bond0-vlan-shared[each.key].id
  subnet            = local.networkmap["shared"].subnet
  mode              = each.value.ip_mode_sh
  ip_address        = each.value.ip_mode_sh == "STATIC" ? each.value.ip_addr_sh : null
}

resource "maas_network_interface_link" "hgx-dpu-bond0-vlan-storage-link" {
  for_each          = {for k,v in local.hgx-machines: v.name=>v if v.enabled == true && v.storage == true}
  machine           = maas_machine.hgx-machines[each.key].id
  network_interface = maas_network_interface_vlan.hgx-dpu-bond0-vlan-storage[each.key].id
  subnet            = local.networkmap["storage"].subnet
  mode              = each.value.ip_mode_st
  ip_address        = each.value.ip_mode_st == "STATIC" ? each.value.ip_addr_st : null
}

resource "maas_network_interface_link" "hgx-dpu-bond0-vlan-dedicated-link" {
  for_each          = {for k,v in local.hgx-machines: v.name=>v if v.enabled == true && v.customer != ""}
  machine           = maas_machine.hgx-machines[each.key].id
  network_interface = maas_network_interface_vlan.hgx-dpu-bond0-vlan-dedicated[each.key].id
  subnet            = local.networkmap["${each.value.customer}"].subnet
  mode              = each.value.ip_mode_dd
  ip_address        = each.value.ip_mode_dd == "STATIC" ? each.value.ip_addr_dd : null
}

data "external" "hgx-non-tf-managed-nics-ns" {
  for_each   = {for k,v in local.hgx-machines: v.name=>v if v.enabled == true && v.eastwest == false}
  program    = ["bash", "${path.module}/maas-non-managed-nics.sh", maas_machine.hgx-machines[each.key].id]
  depends_on = [
    maas_network_interface_physical.hgx-dpu-p0,
    maas_network_interface_physical.hgx-dpu-p1,
  ]
}

# data "external" "hgx-non-tf-managed-nics-nsew" {
#   for_each   = {for k,v in local.hgx-machines: v.name=>v if v.enabled == true && v.eastwest == true}
#   program    = ["bash", "${path.module}/maas-non-managed-nics.sh", maas_machine.hgx-machines[each.key].id]
#   depends_on = [
#     maas_network_interface_physical.hgx-ns-p0,
#     maas_network_interface_physical.hgx-ns-p1,
#     maas_network_interface_physical.hgx-ew-p0,
#     maas_network_interface_physical.hgx-ew-p1,
#   ]
# }

data "external" "hgx-delete-non-tf-managed-nics-ns" {
  for_each = {for k,v in local.hgx-machines: v.name=>v if v.enabled == true && v.eastwest == false}
  program  = ["bash", "${path.module}/delete-non-tf-managed-nic.sh", maas_machine.hgx-machines[each.key].id, data.external.hgx-non-tf-managed-nics-ns[each.key].result.nics]
}

# data "external" "hgx-delete-non-tf-managed-nics-nsew" {
#   for_each = {for k,v in local.hgx-machines: v.name=>v if v.enabled == true && v.eastwest == true}
#   program  = ["bash", "${path.module}/delete-non-tf-managed-nic.sh", maas_machine.hgx-machines[each.key].id, data.external.hgx-non-tf-managed-nics-nsew[each.key].result.nics]
# }