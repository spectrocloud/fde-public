#!/bin/bash
# Version 1.0.6
# Changelog:
# - 1.0.6: Now writing out and using /tmp/bf.cfg during DPU BFB flash to include firmware update of BMC controller
# - 1.0.5: Handle multi-plane SuperNIC/ConnectX adapters
# - 1.0.4: Support Spectrum-X 2.1 architecture based on Spectrum-X Operator (OVS with NICs in switchdev mode)
# - 1.0.3: Initial public version

source /opt/spectrocloud/nodeprep.env
LOG_FILE="/var/log/sc-nodeprep.log"
exec > >(tee -a ${LOG_FILE}) 2>&1
export KUBECONFIG=/etc/kubernetes/kubelet.conf
declare -A arrBF
k8s_node=""
total_amount=0
IFS=',' read -r -a rail_map <<< "$rails_pciaddr"
num_rails="${#rail_map[@]}"

if [ -z "$MTU_EW" ]; then MTU_EW=9216; fi
if [ -z "$DPU_BMC_PWD" ]; then DPU_BMC_PWD="0penBmc"; fi

do_log(){
  print_ok() {
    GREEN_COLOR="\033[0;32m"
    DEFAULT="\033[0m"
    echo -e "${GREEN_COLOR}${1:-}${DEFAULT}"
  }
  print_warning() {
    YELLOW_COLOR="\033[33m"
    DEFAULT="\033[0m"
    echo -e "${YELLOW_COLOR}${1:-}${DEFAULT}"
  }
  print_info() {
    BLUE_COLOR="\033[1;34m"
    DEFAULT="\033[0m"
    echo -e "${BLUE_COLOR}${1:-}${DEFAULT}"
  }
  print_fail() {
    RED_COLOR="\033[0;31m"
    DEFAULT="\033[0m"
    echo -e "${RED_COLOR}${1:-}${DEFAULT}"
  }

  type_of_msg=$(echo $*|cut -d" " -f1)
  msg="$(echo $*|cut -d" " -f2-)"
  msg=" [$type_of_msg] `date "+%Y-%m-%d %H:%M:%S %Z"` [$$] $msg "
  case "$type_of_msg" in
    'FATAL') print_fail "$msg" ;;
    'ERROR') print_fail "$msg" ;;
    'WARN') print_warning "$msg" ;;
    'INFO') print_info "$msg" ;;
    'OK') print_ok "$msg" ;;
    *) echo "$msg" ;;
  esac
}

vercomp () {
    if [[ $1 == $2 ]]
    then
        echo 0; return
    fi
    local IFS=.
    local i ver1=($1) ver2=($2)
    # fill empty fields in ver1 with zeros
    for ((i=${#ver1[@]}; i<${#ver2[@]}; i++))
    do
        ver1[i]=0
    done
    for ((i=0; i<${#ver1[@]}; i++))
    do
        if ((10#${ver1[i]:=0} > 10#${ver2[i]:=0}))
        then
            echo 1; return
        fi
        if ((10#${ver1[i]} < 10#${ver2[i]}))
        then
            echo -1; return
        fi
    done
    return 0
}

fn_ensure_nodeprep() {
  if systemctl list-units | grep stylus-agent; then
    do_log "INFO Running in Agent/Appliance mode, nodeprep is controlled by Stylus."
  else
    if grep "/opt/spectrocloud/nodeprep.sh" /etc/rc.local &>/dev/null
    then
      do_log "INFO Ensured that nodeprep is called at system startup."
    else
      do_log "INFO Nodeprep is not yet being called at system startup, configuring..."
      echo '#!/bin/bash' > /etc/rc.local
      echo '/opt/spectrocloud/nodeprep.sh' >> /etc/rc.local
      chmod +x /etc/rc.local
      do_log "OK Ensured that nodeprep is called at system startup."
    fi
  fi
  if [ -f /usr/bin/mlnx_interface_mgr.sh ]; then
    do_log "INFO Mellanox Interface Manager detected, waiting for it to complete initialization..."
    while ! systemctl status system-mlnx_interface_mgr.slice > /dev/null; do
      sleep 2
    done
    sleep 5
  fi
  if [ -x /usr/sbin/netplan ]; then
    do_log "INFO Netplan detected, re-running 'netplan apply' to ensure any defined VLANs are present"
    netplan apply
  fi
}

fn_ensure_state() {
  do_log "INFO Clearing previous Kubelet CPU Manager and Memory Manager states..."
  systemctl stop kubelet
  if [ -f /var/lib/kubelet/cpu_manager_state ]; then rm -f /var/lib/kubelet/cpu_manager_state; fi
  if [ -f /var/lib/kubelet/memory_manager_state ]; then rm -f /var/lib/kubelet/memory_manager_state; fi
  systemctl start kubelet
  sleep 5
  do_log "OK Cleared previous Kubelet CPU Manager and Memory Manager states..."
  do_log "INFO Detecting node name in Kubernetes..."
  if kubectl get node $(hostname) >/dev/null 2>&1; then
    k8s_node=$(hostname)
  elif kubectl get node $(hostname -f) >/dev/null 2>&1; then
    k8s_node=$(hostname -f)
  else
    do_log "FAIL Unable to determine Kubernetes node name!"
    exit 1
  fi
  do_log "OK Kubernetes node name determined as ${k8s_node}"
  do_log "INFO Checking node state..."
  until [ "$(kubectl get node $k8s_node -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == "True" ]; do
    do_log "INFO Node not yet reporting ready state, will retry in 5s"
    sleep 5
  done
  TEST=$(kubectl get node $k8s_node -o jsonpath="{.metadata.labels.spectrocloud\.com/nodeprep}")
  if [ "$TEST" == "" ]
  then
    do_log "INFO Node state not present, setting new node state..."
    kubectl label node $k8s_node --overwrite "spectrocloud.com/nodeprep=init"
  else
    do_log "OK Node state found, parsing state..."
  fi
  STATE=$(kubectl get node $k8s_node -o jsonpath="{.metadata.labels.spectrocloud\.com/nodeprep}")
  do_log "INFO Nodeprep state is: $STATE"
  if ! systemctl list-units | grep stylus-agent && [ -f /etc/kubernetes/admin.conf ] && [ $STATE == "init" ]; then
    do_log "INFO This is a CAPI node, performing additional verification..."
    if [ $(kubectl get node --kubeconfig /etc/kubernetes/admin.conf --no-headers | wc -l) -eq 1 ]; then
      do_log "INFO This is a fresh control plane node, ensuring cert-manager is installed..."
      kubectl taint nodes $k8s_node spectrocloud.com/nodeprep- --kubeconfig /etc/kubernetes/admin.conf
      while [ $(kubectl get secret -n cert-manager -l owner=helm,name=cert-manager,status=deployed --kubeconfig /etc/kubernetes/admin.conf | wc -l) -eq 0 ]; do
        do_log "INFO Cert-manager not yet deployed, waiting for 3 seconds..."
        sleep 3
      done
      kubectl taint nodes $k8s_node spectrocloud.com/nodeprep:NoSchedule --kubeconfig /etc/kubernetes/admin.conf
    fi
    if kubectl get node $k8s_node -o jsonpath='{.spec.taints}' --kubeconfig /etc/kubernetes/admin.conf | grep "spectrocloud.com/nodeprep"; then
      do_log "INFO This is a control plane node, untainting node and waiting for $CP_DELAY seconds before performing node prep actions..."
      kubectl taint nodes $k8s_node spectrocloud.com/nodeprep- --kubeconfig /etc/kubernetes/admin.conf
      sleep $CP_DELAY
      do_log "INFO Wait over, retainting node"
      kubectl taint nodes $k8s_node spectrocloud.com/nodeprep:NoSchedule --kubeconfig /etc/kubernetes/admin.conf
    fi
    do_log "INFO CAPI node state verified"
  fi
}

fn_update_state() {
  local next_state=$1
  local next_step=$2
  do_log "INFO Setting next stage to $next_state..."
  kubectl label node $k8s_node --overwrite "spectrocloud.com/nodeprep=$next_state"
  if [ "$next_step" == "reboot" ]
  then
    do_log "INFO Reboot requested, scheduling node reboot in 1 minute"
    shutdown -r +1
  fi
}

fn_process_result() {
  if [ $1 -eq 0 ]; then
    do_log "OK Succeeded: $2"
  else
    do_log "FATAL Failed: $2"
    exit 1
  fi
}

fn_inventory_hw() {
  if [ "$LINKTYPE_EW" == "1" ] && systemctl status openibd | grep "Active: failed"; then
    systemctl restart openibd
  fi
  local n=0
  if ! mst status | grep "MST PCI configuration module loaded"; then mst start; fi
  for pci in $(mst status -v | grep -i -E "(bluefield|connectx)" | awk '{print $3}'); do
    ((n++))
    arrBF[$n,0]="$pci"
    arrBF[$n,1]="${pci/\.*/}"
    arrBF[$n,2]="0000:${arrBF[$n,0]}"
    arrBF[$n,3]="0000:${arrBF[$n,1]}"
    arrBF[$n,13]=$(mst status -v | grep "${arrBF[$n,1]}" | wc -l)

    local DESCR=$(mlxconfig -d "0000:${pci}" q INTERNAL_CPU_OFFLOAD_ENGINE | grep "Description:")
    if [ $(echo $DESCR | awk '{print $2}') == "N/A" ]; then
      local DESCR=$(mlxconfig -d "0000:${pci}" q INTERNAL_CPU_OFFLOAD_ENGINE | grep "Device type:")
      arrBF[$n,10]="Air"
    else
      arrBF[$n,10]="Physical"
    fi
    if echo "$DESCR" | grep "SuperNIC" >/dev/null; then
      arrBF[$n,4]="SuperNIC"
      arrBF[$n,14]="true"
      for i in $(seq 0 $((num_rails - 1))); do
        if [ "${arrBF[$n,13]}" -eq 1 ]; then
          if [ "${rail_map[$i]}" == "${arrBF[$n,1]}" ]; then arrBF[$n,11]="r$i"; fi
        else
          if [ "${rail_map[$i]}" == "${arrBF[$n,1]}" ]; then arrBF[$n,11]="r${i}_p${pci/*\./}"; fi
        fi
      done
    elif echo "$DESCR" | grep "DPU" >/dev/null; then
      arrBF[$n,4]="DPU"
      arrBF[$n,11]="dpu"
      if [ "${pci/*\./}" == "0" ]; then arrBF[$n,14]="true"; else arrBF[$n,14]="false"; fi
    elif echo "$DESCR" | grep "ConnectX" >/dev/null; then
      arrBF[$n,4]=$(echo "$DESCR" | awk '{print $2}')
      arrBF[$n,14]="true"
      for i in $(seq 0 $((num_rails - 1))); do
        if [ "${arrBF[$n,13]}" -eq 1 ]; then
          if [ "${rail_map[$i]}" == "${arrBF[$n,1]}" ]; then arrBF[$n,11]="r$i"; fi
        else
          if [ "${rail_map[$i]}" == "${arrBF[$n,1]}" ]; then arrBF[$n,11]="r${i}_p${pci/*\./}"; fi
        fi
      done
    else
      arrBF[$n,4]="Unknown"
      arrBF[$n,14]="false"
    fi

    for dir in $(find /dev -type d -name "rshim*"); do
      local RSHIMPCI=$(cat $dir/misc | grep DEV_NAME | awk '{print $2}' | awk -F "." '{print $1}')
      if [ "$RSHIMPCI" == "pcie-${arrBF[$n,3]}" ]; then
        arrBF[$n,5]="$dir"
      fi
    done

    arrBF[$n,6]=$(for net in $(ls "/sys/bus/pci/devices/${arrBF[$n,2]}/net/"); do if [[ ! $(cat "/sys/bus/pci/devices/${arrBF[$n,2]}/net/$net/phys_port_name" 2> /dev/null) =~ vf ]]; then echo "$net"; fi; done)
    arrBF[$n,7]=$(flint -d "${arrBF[$n,2]}" q | grep "FW Version:" | awk '{print $3}')
    arrBF[$n,8]="$(cat /sys/bus/pci/devices/${arrBF[$n,2]}/current_link_width)x"
    arrBF[$n,9]="$(cat /sys/bus/pci/devices/${arrBF[$n,2]}/max_link_speed | awk '{print $1}')GTs"
    arrBF[$n,12]="$(flint -d ${arrBF[$n,2]} query | grep PSID | awk '{print $2}')"
    do_log "INFO Detected NIC ${arrBF[$n,6]} on PCI address: ${arrBF[$n,0]} of type ${arrBF[$n,4]} with firmware ${arrBF[$n,7]}"
  done
  total_amount=$n
  do_log "INFO Adding NIC firmware info to node labels (spectrocloud.com/nic-xx)..."
  nic_n=0
  for i in $(seq 1 $total_amount); do
    if [ "${arrBF[$i,14]}" == "true" ]; then
      ((nic_n++))
      kubectl label node $k8s_node --overwrite "spectrocloud.com/nic-$nic_n-type=${arrBF[$i,4]}" > /dev/null
      kubectl label node $k8s_node --overwrite "spectrocloud.com/nic-$nic_n-fw=${arrBF[$i,7]}" > /dev/null
      kubectl label node $k8s_node --overwrite "spectrocloud.com/nic-$nic_n-psid=${arrBF[$i,12]}" > /dev/null
      kubectl label node $k8s_node --overwrite "spectrocloud.com/nic-$nic_n-addr=${arrBF[$i,1]/:/\.}" > /dev/null
      kubectl label node $k8s_node --overwrite "spectrocloud.com/nic-$nic_n-name=${arrBF[$i,6]}" > /dev/null
      kubectl label node $k8s_node --overwrite "spectrocloud.com/nic-$nic_n-linkwidth=${arrBF[$i,8]}" > /dev/null
      kubectl label node $k8s_node --overwrite "spectrocloud.com/nic-$nic_n-linkspeed=${arrBF[$i,9]}" > /dev/null
      kubectl label node $k8s_node --overwrite "spectrocloud.com/nic-$nic_n-rail=${arrBF[$i,11]}" > /dev/null
    fi
  done
  gpu_n=0
  lspci -D | grep -E "3D controller: NVIDIA" | while read device; do
    ((gpu_n++))
    bus=$(echo $device | awk '{print $1}')
    sysfs_path="/sys/bus/pci/devices/$bus"
    if [ -f "$sysfs_path/max_link_speed" ]; then
      gpu_name=$(echo $device | sed 's/.*: //' | sed 's/ (rev.*//')
      speed="$(cat $sysfs_path/max_link_speed | awk '{print $1}')GTs"
      width="$(cat $sysfs_path/current_link_width)x"
      do_log "INFO Detected GPU $gpu_n ($gpu_name) on $width PCI link width and max link speed of $speed"
      do_log "INFO Adding PCIe link info for GPU $gpu_n to to node labels..."
      kubectl label node $k8s_node --overwrite "spectrocloud.com/gpu-${gpu_n}-linkspeed=${speed}" > /dev/null
      kubectl label node $k8s_node --overwrite "spectrocloud.com/gpu-${gpu_n}-linkwidth=${width}" > /dev/null
    fi
  done
}

fn_init_sw_stage() {
  do_log "INFO Init stage: Install prereqs, flash BFB..."

  if ! [[ -d /bfb && -d /scripts ]]; then
    do_log "INFO Create /opt/spectrocloud/spcx/bfb directory"
    mkdir -p /opt/spectrocloud/spcx/bfb
    fn_process_result $? "Create /opt/spectrocloud/spcx/bfb directory"
  else
    do_log "OK Directory /opt/spectrocloud/spcx/bfb is present"
  fi

  if ! [ -f /opt/spectrocloud/spcx/bfb/$BFB ]; then
    do_log "INFO Download $MAAS/rcp/firmware/bfb/$BFB to /opt/spectrocloud/spcx/bfb/"
    wget --retry-on-host-error --no-verbose -t 5 "$MAAS/rcp/firmware/bfb/$BFB" -O /opt/spectrocloud/spcx/bfb/$BFB.tmp && mv /opt/spectrocloud/spcx/bfb/$BFB.tmp /opt/spectrocloud/spcx/bfb/$BFB
    fn_process_result $? "Download $MAAS/rcp/firmware/bfb/$BFB to /opt/spectrocloud/spcx/bfb/"
  else
    do_log "OK /opt/spectrocloud/spcx/bfb/$BFB is present"
  fi

  if ! [ -f /opt/spectrocloud/spcx/$DOCA_DEB ]; then
    do_log "INFO Retrieve DOCA repo package from $MAAS/rcp/$DOCA_DEB"
    wget --retry-on-host-error --no-verbose -t 5 "$MAAS/rcp/$DOCA_DEB" -O /opt/spectrocloud/spcx/$DOCA_DEB.tmp && mv /opt/spectrocloud/spcx/$DOCA_DEB.tmp /opt/spectrocloud/spcx/$DOCA_DEB
    fn_process_result $? "Retrieve DOCA repo package from $MAAS/rcp/$DOCA_DEB"
  else
    do_log "OK /opt/spectrocloud/spcx/$DOCA_DEB is present"
  fi

  if $APT_UPDATE; then
    do_log "INFO Updating packages to latest..."
    apt-get update; DEBIAN_FRONTEND=none NEEDRESTART_MODE=l apt-get upgrade -qq -o Dpkg::Options::=--force-confold
    do_log "OK Completed updating packages to latest"
  fi

  if ! [ "$(dpkg -s linux-headers-$(uname -r) | grep -e "^Status: " | awk -F ": " '{print $2}')" == "install ok installed" ]; then
    do_log "INFO Linux headers package for current kernel is missing, installing it now"
    NEEDRESTART_MODE=l apt-get -qq -o Dpkg::Options::=--force-confdef install linux-headers-$(uname -r)
    fn_process_result $? "Install Linux headers package for current kernel"
    apt-mark hold linux-headers-$(uname -r)
    fn_process_result $? "Marked Linux headers package for current kernel as held"
  else
    do_log "OK Linux headers package for current kernel is present"
  fi

  if ! which gcc-12; then
    do_log "INFO GCC-12 not present on the system and needed for DOCA, installing GCC-12"
    NEEDRESTART_MODE=l apt-get -qq -o Dpkg::Options::=--force-confdef install gcc-12 libgcc-12-dev
    fn_process_result $? "Install GCC-12"
  else
    do_log "OK GCC-12 is installed"
  fi

  if ! [ "$(dpkg -s doca-host | grep -e "^Status: " | awk -F ": " '{print $2}')" == "install ok installed" ]; then
    do_log "INFO Install DOCA repo package from /opt/spectrocloud/spcx/$DOCA_DEB"
    dpkg -i /opt/spectrocloud/spcx/$DOCA_DEB
    fn_process_result $? "Install DOCA repo package from /opt/spectrocloud/spcx/$DOCA_DEB"
    apt-get -qq update
    fn_process_result $? "Update APT after install DOCA repo package"
  else
    do_log "OK DOCA repo package is installed"
  fi

  if ! [ "$(dpkg -s doca-all | grep -e "^Status: " | awk -F ": " '{print $2}')" == "install ok installed" ]; then
    do_log "INFO Install DOCA host software"
    NEEDRESTART_MODE=l apt-get -qq -o Dpkg::Options::=--force-confdef install doca-all lldpd mft netplan.io pv psmisc
    fn_process_result $? "Install DOCA host software"
    do_log "INFO Host reboot is needed after installing DOCA, rebooting now..."
    fn_update_state inithw reboot
    exit 0
  else
    do_log "OK DOCA host package is installed"
    do_log "INFO Ensuring additional packages are installed"
    NEEDRESTART_MODE=l apt-get -qq -o Dpkg::Options::=--force-confdef install lldpd mft netplan.io pv psmisc
    fn_process_result $? "Ensure additional packages"
  fi

  do_log "INFO Configure LLDP"
  mkdir -p /etc/lldpd.d
  echo "configure system hostname ." > /etc/lldpd.d/rcp-lldpd.conf
  echo "configure lldp portidsubtype iframe" >> /etc/lldpd.d/rcp-lldpd.conf
  chmod 644 /etc/lldpd.d/rcp-lldpd.conf
  systemctl enable lldpd
  fn_process_result $? "Configure LLDP"

  declare -A arrGrub
  local g=0

  if [ $NUMVF_EW -gt 0 ] || [ $NUMVF_NS -gt 0 ]; then
    do_log "INFO VFs requested, ensuring that SRIOV is enabled on next boot..."
    # Check if CPU is Intel or AMD, set correct IOMMU parameter for each
    if lscpu | grep "Model name" | grep -i intel > /dev/null; then
      do_log "INFO Adding intel_iommu=on to GRUB config..."
      ((g++))
      arrGrub[$g,0]="intel_iommu"
      arrGrub[$g,1]="on"
    elif lscpu | grep "Model name" | grep -i amd > /dev/null; then
      do_log "INFO Adding amd_iommu=on to GRUB config..."
      ((g++))
      arrGrub[$g,0]="amd_iommu"
      arrGrub[$g,1]="on"
    fi

    # Set IOMMU Passthrough for SRIOV
    if ! cat /etc/default/grub | grep GRUB_CMDLINE_LINUX_DEFAULT | grep iommu=pt > /dev/null; then
      do_log "INFO Adding iommu=pt to GRUB config..."
      ((g++))
      arrGrub[$g,0]="iommu"
      arrGrub[$g,1]="pt"
    fi

    # Enable RDMA namespace awareness for proper SR-IOV isolation
    echo "options ib_core netns_mode=0" > /etc/modprobe.d/ib_core.conf
    do_log "INFO Configured 'options ib_core netns_mode=0' in /etc/modprobe.d/ib_core.conf"
  fi

  if [ $HP2M -gt 0 ] || [ $HP1G -gt 0 ]; then
    do_log "INFO Hugepages requested, ensuring that they are requested on next boot..."
    ((g++))
    arrGrub[$g,0]="default_hugepagesz"
    arrGrub[$g,1]="$HPDEFAULT"
    if [ $HP2M -gt 0 ]; then
      ((g++))
      arrGrub[$g,0]="hugepagesz"
      arrGrub[$g,1]="2M"
      ((g++))
      arrGrub[$g,0]="hugepages"
      arrGrub[$g,1]="$HP2M"
    fi
    if [ $HP1G -gt 0 ]; then
      ((g++))
      arrGrub[$g,0]="hugepagesz"
      arrGrub[$g,1]="1G"
      ((g++))
      arrGrub[$g,0]="hugepages"
      arrGrub[$g,1]="$HP1G"
    fi
  fi

  if [ $g -gt 0 ]; then
    local GRUBCFGCLEAN=()
    local GRUBCFGNEW=()
    GRUBCFGCLEAN+=('GRUB_CMDLINE_LINUX="$(printf '"'"'%s'"'"' "$GRUB_CMDLINE_LINUX" | sed -E')
    GRUBCFGNEW+=('GRUB_CMDLINE_LINUX="$GRUB_CMDLINE_LINUX')
    for i in $(seq 1 $g); do
      if [ $i -eq 1 ]; then
        GRUBCFGCLEAN+=("'s/(^|[[:space:]])${arrGrub[$i,0]}=[^[:space:]]*//g;")
        GRUBCFGNEW+=("${arrGrub[$i,0]}=${arrGrub[$i,1]}")
      elif [ $i -eq $g ]; then
        GRUBCFGCLEAN+=("s/(^|[[:space:]])${arrGrub[$i,0]}=[^[:space:]]*//g')\"")
        GRUBCFGNEW+=("${arrGrub[$i,0]}=${arrGrub[$i,1]}\"")
      else
        GRUBCFGCLEAN+=("s/(^|[[:space:]])${arrGrub[$i,0]}=[^[:space:]]*//g;")
        GRUBCFGNEW+=("${arrGrub[$i,0]}=${arrGrub[$i,1]}")
      fi
    done

    echo 'GRUB_CMDLINE_LINUX=" $GRUB_CMDLINE_LINUX "' > /etc/default/grub.d/90-nodeprep.cfg
    echo "${GRUBCFGCLEAN[@]}" >> /etc/default/grub.d/90-nodeprep.cfg
    echo "${GRUBCFGNEW[@]}" >> /etc/default/grub.d/90-nodeprep.cfg
    echo 'GRUB_CMDLINE_LINUX="$(echo "$GRUB_CMDLINE_LINUX" | xargs)"' >> /etc/default/grub.d/90-nodeprep.cfg

    do_log "INFO GRUB config changed, running update-grub..."
    update-grub
  fi

  do_log "INFO Enable and restart rshim"
  systemctl daemon-reload && systemctl enable rshim && systemctl restart rshim
  fn_process_result $? "Enable and restart rshim"
  do_log "INFO Sleeping for 10 seconds to allow rshim to initialize..."
  sleep 10

  do_log "INFO Verify rshim is running"
  systemctl status rshim | grep "Active: active (running)"
  fn_process_result $? "Verify rshim is running"

  if $APT_UPDATE; then
    do_log "INFO Running APT package cleanup..."
    apt-get autoremove -qq
  fi
}

fn_init_hw_stage() {
  do_log "INFO Flash BFB to Bluefield-3 adapters"
  local pids=()
  for i in $(seq 1 $total_amount); do
    if [ "${arrBF[$i,4]}" == "SuperNIC" ]; then
      if [ $(vercomp "${arrBF[$i,7]}" "$BFB_FW") -ge 0 ]; then
        do_log "OK Bluefield-3 firmware ${arrBF[$i,7]} already matches or supersedes version $BFB_FW, skipping flash."
      else
        do_log "INFO Starting Bluefield-3 firmware flash to ${arrBF[$i,2]}..."
        NEEDREBOOT=true
        local rshim_addr="${arrBF[$i,5]/\/dev\//}"
        bfb-install --rshim $rshim_addr --bfb /opt/spectrocloud/spcx/bfb/$BFB --verbose & pids+=("$!")
      fi
    elif [ "${arrBF[$i,4]}" == "DPU" ]; then
      if $CONTROLDPU; then
        if [ $(vercomp "${arrBF[$i,7]}" "$BFB_FW") -ge 0 ]; then
          do_log "OK Bluefield-3 firmware ${arrBF[$i,7]} already matches or supersedes expected version $BFB_FW, skipping flash."
        else
          do_log "INFO Starting Bluefield-3 firmware flash to ${arrBF[$i,2]} in background..."
          NEEDREBOOT=true
          local rshim_addr="${arrBF[$i,5]/\/dev\//}"
          echo -e "BMC_USER=\"root\"\nBMC_PASSWORD=\"$DPU_BMC_PWD\"\nUPDATE_BMC_FW=\"yes\"\nBMC_REBOOT=\"yes\"\nBMC_RESET=\"yes\"\nUPDATE_CEC_FW=\"yes\"" > "/tmp/bf.cfg"
          if [ -n "$DPU_BMC_NEW_PWD" ]; then
            if [ ${#DPU_BMC_NEW_PWD} -ge 12 ]; then
              do_log "INFO DPU_BMC_NEW_PWD variable is set and meets minimum length, adding parameter to DPU flash configuration"
              echo -e "NEW_BMC_PASSWORD=\"$DPU_BMC_NEW_PWD\"\n" >> "/tmp/bf.cfg"
            else
              do_log "WARN DPU_BMC_NEW_PWD variable is set but is less than 12 characters, skipping this parameter as it will be denied by the BMC"
            fi
          fi
          bfb-install -c /tmp/bf.cfg --rshim $rshim_addr --bfb /opt/spectrocloud/spcx/bfb/$BFB --verbose & pids+=("$!")
        fi
      else
        do_log "INFO Control of DPUs is not allowed by policy, skipping DPU ${arrBF[$i,2]}"
      fi
    elif [[ "${arrBF[$i,4]}" =~ ConnectX ]]; then
      do_log "INFO Firmware flashing of ConnectX adapters is not implemented, skipping ConnectX NIC ${arrBF[$i,2]}"
    fi
  done
  # Wait for flashing to complete and check for errors
  local rc=0
  for pid in ${pids[@]}; do
      if ! wait $pid; then rc=1; fi
  done
  fn_process_result $rc "Flash BFB to Bluefield-3 adapters in parallel"
}

fn_config_stage(){
  do_log "INFO Configure network adapter firmware"
  if [ $NUMVF_EW -gt 1 ]; then CNX_NUM_OF_VFS=$NUMVF_EW; else CNX_NUM_OF_VFS=1; fi
  if [ $NUMVF_NS -gt 1 ]; then DPU_NUM_OF_VFS=$NUMVF_NS; else DPU_NUM_OF_VFS=1; fi
  for i in $(seq 1 $total_amount); do
    local FLASHCONFIG=false
    local FLASH=()
    if [ "$LINKTYPE_EW" == "2" ]; then
      FLASH+=("ROCE_RTT_RESP_DSCP_P1=48")
      FLASH+=("ROCE_RTT_RESP_DSCP_MODE_P1=1")
      if [ "${arrBF[$i,4]}" == "SuperNIC" ] || [[ "${arrBF[$i,4]}" =~ ConnectX-[7-9] ]]; then
        FLASH+=("ROCE_ADAPTIVE_ROUTING_EN=$ROCECC")
        FLASH+=("USER_PROGRAMMABLE_CC=$ROCECC")
        FLASH+=("TX_SCHEDULER_LOCALITY_MODE=2")
        FLASH+=("ROCE_CC_STEERING_EXT=2")
      fi
    fi
    FLASH+=("SRIOV_EN=1")
    if [ "${arrBF[$i,4]}" == "SuperNIC" ] && [ "${arrBF[$i,10]}" == "Physical" ]; then
      FLASH+=("LINK_TYPE_P1=$LINKTYPE_EW")
      FLASH+=("NUM_OF_VFS=$CNX_NUM_OF_VFS")
      if [ "$LINKTYPE_EW" == "2" ]; then FLASH+=("MULTIPATH_DSCP=0"); fi
      if /usr/bin/mlxconfig -d "${arrBF[$i,2]}" q LINK_TYPE_P2 >/dev/null; then
        FLASH+=("LINK_TYPE_P2=$LINKTYPE_EW")
        if [ "$LINKTYPE_EW" == "2" ]; then
          FLASH+=("ROCE_RTT_RESP_DSCP_P2=48")
          FLASH+=("ROCE_RTT_RESP_DSCP_MODE_P2=1")
        fi
      fi
      FLASHCONFIG=true
    elif [[ "${arrBF[$i,4]}" =~ ConnectX ]] && [ "${arrBF[$i,10]}" == "Physical" ]; then
      if /usr/bin/mlxconfig -d "${arrBF[$i,2]}" q LINK_TYPE_P1 >/dev/null; then FLASH+=("LINK_TYPE_P1=$LINKTYPE_EW"); fi
      FLASH+=("NUM_OF_VFS=$CNX_NUM_OF_VFS")
      if [ "$LINKTYPE_EW" == "2" ] && [[ "${arrBF[$i,4]}" =~ ConnectX-[7-9] ]]; then FLASH+=("MULTIPATH_DSCP=0"); fi
      if /usr/bin/mlxconfig -d "${arrBF[$i,2]}" q LINK_TYPE_P2 >/dev/null; then
        FLASH+=("LINK_TYPE_P2=$LINKTYPE_EW")
        if [ "$LINKTYPE_EW" == "2" ]; then
          FLASH+=("ROCE_RTT_RESP_DSCP_P2=48")
          FLASH+=("ROCE_RTT_RESP_DSCP_MODE_P2=1")
        fi
      fi
      FLASHCONFIG=true
    elif [[ "${arrBF[$i,4]}" =~ ConnectX ]] && [ "${arrBF[$i,10]}" == "Air" ]; then
      FLASH+=("LINK_TYPE_P1=$LINKTYPE_EW")
      FLASH+=("NUM_OF_VFS=$CNX_NUM_OF_VFS")
      if [ "$LINKTYPE_EW" == "2" ]; then FLASH+=("ROCE_CC_RTT_TIMESTAMP_FORMAT=0"); fi
      FLASHCONFIG=true
    elif [ "${arrBF[$i,4]}" == "DPU" ]; then
      FLASH+=("LINK_TYPE_P1=$LINKTYPE_NS")
      FLASH+=("NUM_OF_VFS=$DPU_NUM_OF_VFS")
      if [ "$LINKTYPE_NS" == "2" ]; then FLASH+=("MULTIPATH_DSCP=0"); fi
      FLASH+=("INTERNAL_CPU_OFFLOAD_ENGINE=$DPUOFFLOAD")
      if /usr/bin/mlxconfig -d "${arrBF[$i,2]}" q LINK_TYPE_P2 >/dev/null; then
        FLASH+=("LINK_TYPE_P2=$LINKTYPE_NS")
        if [ "$LINKTYPE_NS" == "2" ]; then
          FLASH+=("ROCE_RTT_RESP_DSCP_P2=48")
          FLASH+=("ROCE_RTT_RESP_DSCP_MODE_P2=1")
        fi
      fi
      if $CONTROLDPU; then
        FLASHCONFIG=true
      else
        do_log "INFO Control of DPUs is not allowed by policy, skipping DPU ${arrBF[$i,2]}"
      fi
    fi
    if [ "$FLASHCONFIG" == "true" ]; then
      /usr/bin/mlxconfig -d "${arrBF[$i,2]}" -y reset
      /usr/bin/mlxconfig -d "${arrBF[$i,2]}" -y set "${FLASH[@]}"
      fn_process_result $? "Configure ${arrBF[$i,4]} adapter firmware for ${arrBF[$i,0]}"
    fi
    if [[ "${arrBF[$i,11]}" =~ ^r[0-9]+ ]]; then
      do_log "INFO Generate netplan file for ${arrBF[$i,6]} to ignore carrier changes"
      echo -e "network:\n  version: 2\n  ethernets:\n    eth_${arrBF[$i,11]}:\n      ignore-carrier: true\n      mtu: ${MTU_EW}" > "/etc/netplan/gpu_fabric_eth_${arrBF[$i,11]}.yaml"
      chmod 0600 "/etc/netplan/gpu_fabric_eth_${arrBF[$i,11]}.yaml"
    fi
  done
}

fn_disable_acs(){
  do_log "INFO Disable ACS on all PCIe switches"
  for BDF in `lspci -d "*:*:*" | awk '{print $1}'`; do
    setpci -v -s ${BDF} ECAP_ACS+0x6.w > /dev/null 2>&1
    if [ $? -ne 0 ]; then
      do_log "INFO ${BDF} does not support ACS, skipping"
      continue
    fi
    do_log "OK Disabling ACS on ${BDF}"
    setpci -v -s ${BDF} ECAP_ACS+0x6.w=0000
    if [ $? -ne 0 ]; then
      do_log "ERROR ${BDF} Error disabling ACS on ${BDF}"
      continue
    fi
    local NEW_VAL=`setpci -v -s ${BDF} ECAP_ACS+0x6.w | awk '{print $NF}'`
    if [ "${NEW_VAL}" != "0000" ]; then
      do_log "ERROR Failed to disable ACS on ${BDF}"
      continue
    fi
  done
}

fn_set_vfs(){
  if [ $NUMVF_EW -gt 0 ] || [ $NUMVF_NS -gt 0 ]; then
    do_log "INFO Create VFs on network adapters"

    # Cordon node during procedure
    do_log "INFO Cordoning node for VF configuration..."
    kubectl cordon $k8s_node

    if [ -f /etc/udev/rules.d/71-persistent-net-vf.rules ]; then rm /etc/udev/rules.d/71-persistent-net-vf.rules; fi
    if [ -f /etc/udev/rules.d/61-persistent-rdma-vf.rules ]; then rm /etc/udev/rules.d/61-persistent-rdma-vf.rules; fi
    for i in $(seq 1 $total_amount); do
      if [ "${arrBF[$i,4]}" == "SuperNIC" ] || [[ "${arrBF[$i,4]}" =~ ConnectX ]] || [ "${arrBF[$i,4]}" == "DPU" ]; then
        local netif="${arrBF[$i,6]}"

        if ([ "${arrBF[$i,4]}" == "SuperNIC" ] || [[ "${arrBF[$i,4]}" =~ ConnectX ]]) && [ $NUMVF_EW -gt 0 ] && [ "$ESWITCH_MODE" == "switchdev" ]; then
          ((total_eswitches++))
          do_log "INFO Ensure eSwitch is set to switchdev mode for $netif"
          if devlink dev eswitch show pci/"${arrBF[$i,2]}" | grep legacy >/dev/null; then
            echo 0 > "/sys/class/net/$netif/device/sriov_numvfs"
            devlink dev eswitch set pci/"${arrBF[$i,2]}" mode legacy
            devlink dev param set pci/"${arrBF[$i,2]}" name flow_steering_mode value hmfs cmode runtime
            devlink dev eswitch set pci/"${arrBF[$i,2]}" mode switchdev
          fi
        fi

        if ! [ "$(ip -br link show dev $netif | awk '{print $2}')" == "UP" ]; then
          do_log "INFO Interface $netif is not in UP state, setting link to UP..."
          ip link set $netif up
          fn_process_result $? "Set interface $netif to UP state"
        fi

        if ([ "${arrBF[$i,4]}" == "SuperNIC" ] || [[ "${arrBF[$i,4]}" =~ ConnectX ]]) && [ $NUMVF_EW -gt 0 ]; then
          do_log "INFO Setting sriov_numvfs to $NUMVF_EW for ${arrBF[$i,4]} $netif"
          echo "$NUMVF_EW" > "/sys/class/net/$netif/device/sriov_numvfs"
          fn_process_result $? "Create $NUMVF_EW VFs for ${arrBF[$i,4]} $netif"
        elif [ "${arrBF[$i,4]}" == "DPU" ] && [ $NUMVF_NS -gt 0 ]; then
          do_log "INFO Setting sriov_numvfs to $NUMVF_NS for DPU $netif"
          echo "$NUMVF_NS" > "/sys/class/net/$netif/device/sriov_numvfs"
          fn_process_result $? "Create $NUMVF_NS VFs for DPU $netif"
        fi

        # Wait for VFs to be created
        sleep 2

        if ([ "${arrBF[$i,4]}" == "SuperNIC" ] || [[ "${arrBF[$i,4]}" =~ ConnectX ]]) ; then
          if [ "$LINKTYPE_EW" == "1" ]; then
            LINKTYPE="IB"
          else
            LINKTYPE="ETH"
          fi
          LINKVFS=$NUMVF_EW
        elif [ "${arrBF[$i,4]}" == "DPU" ] ; then
          if [ "$LINKTYPE_NS" == "1" ]; then
            LINKTYPE="IB"
          else
            LINKTYPE="ETH"
          fi
          LINKVFS=$NUMVF_NS
        fi

        if [ $LINKVFS -gt 0 ]; then
          # VFs requested, we need to do additional port configuration...
          do_log "INFO VFs requested, proceding to configure VF GUIDs..."

          # Find InfiniBand device name
          local ib_dev=""
          if [ -d "/sys/class/net/$netif/device/infiniband" ]; then
            ib_dev=$(ls "/sys/class/net/$netif/device/infiniband" | head -n1)
            do_log "INFO InfiniBand device for $netif is $ib_dev"
          else
            do_log "ERROR Could not find InfiniBand device for $netif"
            continue
          fi

          local nic_guid_src=$(cat /sys/class/net/$netif/device/infiniband/$ib_dev/node_guid)
          local nic_guid_raw=${nic_guid_src//:/}

          # Configure GUIDs for each VF
          for vf in $(seq 0 $((LINKVFS - 1))); do
            local vf_hex=$(printf "%02x" $vf)
            local node_guid=$(echo "${nic_guid_raw:0:7}f1${vf_hex}${nic_guid_raw:11:16}" | sed 's/../&:/g; s/:$//')
            local port_guid=$(echo "${nic_guid_raw:0:7}f2${vf_hex}${nic_guid_raw:11:16}" | sed 's/../&:/g; s/:$//')
            local mac_addr=$(echo "f2${vf_hex}${nic_guid_raw:8:8}" | sed 's/../&:/g; s/:$//')
            local sriov_path="/sys/class/infiniband/$ib_dev/device/sriov/$vf"

            if [ ! -d "$sriov_path" ]; then
              do_log "ERROR sriov path $sriov_path does not exist for VF $vf"
              continue
            fi

            # Set Node GUID
            do_log "INFO Setting Node GUID for $ib_dev VF $vf to $node_guid"
            echo "$node_guid" > "$sriov_path/node"
            if [ $? -eq 0 ]; then
              do_log "OK Node GUID set successfully"
            else
              do_log "ERROR Failed to set Node GUID for VF $vf"
              continue
            fi

            # Set Port GUID or MAC address, depending on linktype
            if [ "$LINKTYPE" == "IB" ]; then
              do_log "INFO Setting Port GUID for $ib_dev VF $vf to $port_guid"
              echo "$port_guid" > "$sriov_path/port"
              if [ $? -eq 0 ]; then
                do_log "OK Port GUID set successfully"
              else
                do_log "ERROR Failed to set Port GUID for VF $vf"
                continue
              fi
            elif [ ! "$ESWITCH_MODE" == "switchdev" ]; then
              do_log "INFO Setting MAC address for $ib_dev VF $vf to $mac_addr"
              echo "$mac_addr" > "$sriov_path/mac"
              if [ $? -eq 0 ]; then
                do_log "OK MAC address set successfully"
              else
                do_log "ERROR Failed to set MAC address for VF $vf"
                continue
              fi
            fi

            # Set policy to Follow (mirror physical port state) for IB devices
            if [ "$LINKTYPE" == "IB" ]; then
              do_log "INFO Setting policy to Follow for $ib_dev VF $vf"
              echo "Follow" > "$sriov_path/policy"
              if [ $? -eq 0 ]; then
                do_log "OK Policy set to Follow"
              else
                do_log "WARN Failed to set policy for VF $vf (non-critical)"
              fi
            fi

            # Unbind and rebind VF to make GUID changes active
            do_log "INFO Unbinding and rebinding VF $vf on $ib_dev to activate new GUIDs"
            local VF_PCI_ADDR=$(cat /sys/class/infiniband/$ib_dev/device/virtfn$vf/uevent | grep PCI_SLOT_NAME | awk -F '=' '{print $2}')
            echo "$VF_PCI_ADDR" > /sys/bus/pci/drivers/mlx5_core/unbind
            echo "$VF_PCI_ADDR" > /sys/bus/pci/drivers/mlx5_core/bind

            # Verify GUIDs were set
            local set_node_guid=$(cat "$sriov_path/node" 2>/dev/null)
            if [ "$LINKTYPE" == "IB" ]; then
              local set_port_guid=$(cat "$sriov_path/port" 2>/dev/null)
              do_log "INFO VF $vf verification - Node: $set_node_guid, Port: $set_port_guid"
            else
              do_log "INFO VF $vf verification - Node: $set_node_guid"
            fi

            # Write udev rename rule
            if [ "$LINKTYPE" == "ETH" ] && [[ "${arrBF[$i,11]}" =~ ^r[0-9]+ ]]; then
              echo "ACTION==\"add\", KERNELS==\"$VF_PCI_ADDR\", SUBSYSTEM==\"net\", NAME=\"nic_vf${vf}_${arrBF[$i,11]}\"" >> /etc/udev/rules.d/71-persistent-net-vf.rules
              echo "ACTION==\"add\", KERNELS==\"$VF_PCI_ADDR\", SUBSYSTEM==\"infiniband\", PROGRAM=\"rdma_rename %k NAME_FIXED roce_vf${vf}_${arrBF[$i,11]}\"" >> /etc/udev/rules.d/61-persistent-rdma-vf.rules
              local VF_REP=$(devlink port show | grep "pci/${arrBF[$i,2]}" | grep pcivf | awk '{print $5}')
              local VF_REP_PORTNAME=$(cat /sys/class/net/$VF_REP/phys_port_name)
              local VF_REP_SWITCHID=$(cat /sys/class/net/$VF_REP/phys_switch_id)
              echo "ACTION==\"add\", ATTR{phys_switch_id}==\"$VF_REP_SWITCHID\", ATTR{phys_port_name}==\"$VF_REP_PORTNAME\" SUBSYSTEM==\"net\", NAME=\"nic_vf${vf}_rep_${arrBF[$i,11]}\"" >> /etc/udev/rules.d/71-persistent-net-vf.rules
            fi
          done

          do_log "OK Configured GUIDs for $LINKVFS VFs on $netif ($ib_dev)"
        fi
      fi
    done

    if [ "$ESWITCH_MODE" == "switchdev" ]; then
      # Configure DOCA OVS
      systemctl stop openvswitch-switch
      rm /var/lib/openvswitch/conf.db
      systemctl start openvswitch-switch
      ovs-vsctl set Open_vSwitch . other_config:doca-init=true
      ovs-vsctl set Open_vSwitch . other_config:hw-offload=true
      ovs-vsctl set Open_vSwitch . other_config:hw-offload-ct-size=0
      ovs-vsctl set Open_vSwitch . other_config:max-idle=300000
      ovs-vsctl set Open_vSwitch . other_config:doca-eswitch-max=$num_rails
      systemctl restart openvswitch-switch
      for r in $(seq 0 $((num_rails - 1))); do
        ovs-vsctl --may-exist add-br br-rail-r$r -- set br br-rail-r$r fail-mode=secure datapath_type=netdev
        ip link set dev br-rail-r$r mtu $(($MTU_EW-50)) up
      done
    fi
    do_log "OK VFs on network adapters created"
  fi
}

fn_rename_devices(){
  if [ "$LINKTYPE_EW" == "2" ]; then
    # Rename each PF and RDMA device to eth_r[rail-id]
    if [ -f /etc/udev/rules.d/70-persistent-net.rules ]; then rm /etc/udev/rules.d/70-persistent-net.rules; fi
    if [ -f /etc/udev/rules.d/60-persistent-rdma.rules ]; then rm /etc/udev/rules.d/60-persistent-rdma.rules; fi
    for i in $(seq 1 $total_amount); do
      if [[ "${arrBF[$i,11]}" =~ ^r[0-9]+ ]]; then
        ip link set dev "${arrBF[$i,6]}" down
        echo "ACTION==\"add\", KERNELS==\"${arrBF[$i,2]}\", SUBSYSTEM==\"net\", NAME=\"eth_${arrBF[$i,11]}\"" >> /etc/udev/rules.d/70-persistent-net.rules
        echo "ACTION==\"add\", KERNELS==\"${arrBF[$i,2]}\", SUBSYSTEM==\"infiniband\", PROGRAM=\"rdma_rename %k NAME_FIXED roce_${arrBF[$i,11]}\"" >> /etc/udev/rules.d/60-persistent-rdma.rules
      fi
    done
    do_log "INFO Trigger udev rules reload and run triggers"
    udevadm control --reload
    udevadm trigger --action=add --subsystem-match=net
    udevadm trigger --action=add --subsystem-match=infiniband
    sleep 5
    for i in $(seq 1 $total_amount); do
      if [[ "${arrBF[$i,11]}" =~ ^r[0-9]+ ]]; then
        do_log "INFO Bring up eth_${arrBF[$i,11]} PF interface"
        ip link set dev "eth_${arrBF[$i,11]}" mtu $MTU_EW up
        if [ $NUMVF_EW -gt 0 ]; then
          do_log "INFO Bring up VF and VF Representor interfaces for PF eth_${arrBF[$i,11]}"
          for vf in $(seq 0 $((NUMVF_EW - 1))); do
            # Set VF MTU to 50 bytes below max for Ethernet linktype
            if [ "$LINKTYPE_EW" == "2" ]; then
              ip link set dev "nic_vf${vf}_${arrBF[$i,11]}" mtu $(($MTU_EW-50)) up
              ip link set dev "nic_vf${vf}_rep_${arrBF[$i,11]}" mtu $(($MTU_EW-50)) up
            fi
          done
        fi
      fi
    done
    do_log "OK PF, VF and VF Representors renamed."
  fi
}

fn_set_lossless_roce(){
  if [ "$LINKTYPE_EW" == "2" ] && ([[ "${arrBF[$i,4]}" =~ ConnectX-[7-9] ]] || [ "${arrBF[$i,4]}" == "SuperNIC" ]); then
    # Enable lossless RoCE mode for RDMA devices
    for i in $(seq 1 $total_amount); do
      if [[ "${arrBF[$i,11]}" =~ ^r[0-9]+ ]]; then
        if [ $NUMVF_EW -gt 0 ]; then
          for vf in $(seq 0 $((NUMVF_EW - 1))); do
            do_log "INFO Enable lossless RoCE mode for roce_vf${vf}_${arrBF[$i,11]}"
            cma_roce_tos -d "roce_vf${vf}_${arrBF[$i,11]}" -t 96
            echo 96 > "/sys/class/infiniband/roce_vf${vf}_${arrBF[$i,11]}/tc/1/traffic_class"
          done
        else
          do_log "INFO Enable lossless RoCE mode for roce_${arrBF[$i,11]}"
          cma_roce_tos -d "roce_${arrBF[$i,11]}" -t 96
          echo 96 > "/sys/class/infiniband/roce_${arrBF[$i,11]}/tc/1/traffic_class"
        fi
        do_log "INFO Set PFC priority 3 for ${arrBF[$i,6]}"
        mlnx_qos -i "${arrBF[$i,6]}" --pfc=0,0,0,1,0,0,0,0 --trust=dscp > "/var/log/spcx_log_${arrBF[$i,6]}.log"
        do_log "INFO Set ECN for RoCE traffic for ${arrBF[$i,6]}"
        for n in $(seq 0 7); do
          echo 1 > "/sys/class/net/${arrBF[$i,6]}/ecn/roce_rp/enable/$n"
          echo 1 > "/sys/class/net/${arrBF[$i,6]}/ecn/roce_np/enable/$n"
        done
        do_log "INFO Set DSCP value for CNP packets to 48 for ${arrBF[$i,6]}"
        echo 48 > "/sys/class/net/${arrBF[$i,6]}/ecn/roce_np/cnp_dscp"
        do_log "INFO Enable Adaptive Routing for ${arrBF[$i,6]}"
        mlxreg -d "${arrBF[$i,2]}" --reg_name ROCE_ACCL --set roce_adp_retrans_en=0x1,roce_tx_window_en=0x1,roce_slow_restart_en=0x0,roce_slow_restart_idle_en=0x0,adaptive_routing_forced_en=0x1 --yes >> "/var/log/spcx_log_${arrBF[$i,6]}.log"
        do_log "INFO Enable Spectrum-X Congestion Control on roce_${arrBF[$i,11]}"
        nohup /opt/mellanox/doca/tools/doca_spcx_cc -d "roce_${arrBF[$i,11]}" 2>&1 | logger -t "doca_spcx_cc_roce_${arrBF[$i,11]}" &
        sleep 3
        do_log "INFO TODO: Tune Spectrum-X Congestion Control on ${arrBF[$i,6]}"
        #mlxreg -d "${arrBF[$i,6]}" -y --set "cmd_type=8,value=<value>" --reg_name PPCC --indexes "local_port=1,pnat=0,lp_msb=0,algo_slot=0,algo_param_index=<index>"
        do_log "INFO Set Inter Packet Gap on ${arrBF[$i,6]} to 0x00000019"
        mlxreg -d "${arrBF[$i,2]}" -y --set "ipg=0x00000019" --reg_name PIPG --indexes "local_port=1,lp_msb=0,ipg_cap_index=0"
        mlxreg -d "${arrBF[$i,2]}" -y --reg_id 0x5006 --set "0x0.8:4=2,0x0.16:8=1,0x4.8:1=1,0x4.31:1=1" --reg_len 16
        mlxreg -d "${arrBF[$i,2]}" -y --reg_id 0x5006 --set "0x0.8:4=1,0x0.16:8=1,0x4.8:1=1,0x4.31:1=1" --reg_len 16
      fi
    done
  fi
}

fn_add_pfs_to_rail_bridges(){
  if [ "$ESWITCH_MODE" == "switchdev" ]; then
    for i in $(seq 1 $total_amount); do
      if [[ "${arrBF[$i,11]}" =~ ^r[0-9]+ ]]; then
        do_log "INFO Add eth_${arrBF[$i,11]} to OVS bridge br-rail-${arrBF[$i,11]}"
        ovs-vsctl --may-exist add-port "br-rail-${arrBF[$i,11]}" "eth_${arrBF[$i,11]}" -- set int "eth_${arrBF[$i,11]}" mtu_request=$(($MTU_EW-50)) type=dpdk
        do_log "INFO Set eth_${arrBF[$i,11]} interface on OVS to MTU $(($MTU_EW-50))"
        ovs-vsctl set int "eth_${arrBF[$i,11]}" mtu_request=$(($MTU_EW-50))
      fi
    done
  fi
}

fn_restart_sriov_devplugin(){
  # Restart SRIOV Device Plugin to ensure VFs get inventoried correctly and uncordon node
  do_log "INFO Restarting SRIOV Device Plugin to ensure VFs get inventoried correctly..."
  kubectl delete pod -n nvidia-network-operator --field-selector="spec.nodeName=$k8s_node" -l 'app=sriov-device-plugin'
  do_log "INFO Uncordoning node..."
  kubectl uncordon $k8s_node
}

fn_setup_nfsrdma(){
  if [ "$NFSORDMA_ENABLED" != "true" ]; then
    do_log "INFO NFSoRDMA not enabled, skipping..."
    return
  fi
  
  do_log "INFO Ensuring nfs-common package"
  if ! [ "$(dpkg -s nfs-common | grep -e "^Status: " | awk -F ": " '{print $2}')" == "install ok installed" ]; then
    apt install -y nfs-common
  fi

  do_log "INFO Configuring NFSoRDMA kernel module support..."

  # Install OFED-compatible NFSoRDMA DKMS package
  if ! dpkg -s mlnx-nfsrdma-dkms &>/dev/null; then
    do_log "INFO Installing mlnx-nfsrdma-dkms..."
    NEEDRESTART_MODE=l apt-get install -y mlnx-nfsrdma-dkms
    fn_process_result $? "Install mlnx-nfsrdma-dkms"
  else
    do_log "OK mlnx-nfsrdma-dkms is already installed"
  fi

  # Load NFSoRDMA kernel modules
  do_log "INFO Loading NFSoRDMA kernel modules..."
  modprobe rpcrdma 2>/dev/null || true
  modprobe xprtrdma 2>/dev/null || true
  modprobe svcrdma 2>/dev/null || true

  if lsmod | grep -q rpcrdma; then
    do_log "OK rpcrdma module loaded"
  else
    do_log "WARN rpcrdma module not loaded - may need reboot"
  fi

  do_log "INFO Configuring NFSoRDMA module auto-load"
  echo "rpcrdma" > /etc/modules-load.d/nfsrdma.conf
  echo "xprtrdma" >> /etc/modules-load.d/nfsrdma.conf
  echo "svcrdma" >> /etc/modules-load.d/nfsrdma.conf

  do_log "OK NFSoRDMA configuration complete"
}

do_log "OK Spectro Cloud node preparation for Spectrum-X"
fn_ensure_nodeprep
fn_ensure_state

case "$STATE" in
  "inithw")
    ;&
  "init")
    NEEDREBOOT=false
    fn_init_sw_stage
    fn_inventory_hw
    fn_init_hw_stage
    if $NEEDREBOOT; then
      fn_update_state config reboot
    else
      fn_update_state config
      fn_config_stage
      fn_update_state precomplete reboot
    fi ;;

  "config")
    do_log "INFO Config stage: set NIC configs with mlxconfig..."
    fn_inventory_hw
    fn_config_stage
    fn_update_state precomplete reboot ;;

  "precomplete")
    ;&
  "complete")
    do_log "INFO Complete stage: inventory HW and set VFs if requested..."
    if $DISABLE_ACS; then fn_disable_acs; fi
    fn_inventory_hw
    fn_set_vfs
    fn_setup_nfsrdma
    fn_rename_devices
    fn_inventory_hw
    fn_set_lossless_roce
    fn_add_pfs_to_rail_bridges
    fn_restart_sriov_devplugin
    # Temporary workaround for GDS bug in GPU Operator 25.10
    mkdir -p /run/mellanox/drivers
    touch /run/mellanox/drivers/.driver-ready
    # End workaround
    if [ -f /etc/kubernetes/admin.conf ]; then
      # This is a control plane node, it can untaint itself
      kubectl taint nodes $k8s_node spectrocloud.com/nodeprep- --kubeconfig /etc/kubernetes/admin.conf
    fi
    fn_update_state complete
    do_log "OK Nodeprep complete." ;;

  *)
    do_log "ERROR Unknown state: $STATE, aborting..." ;;
esac