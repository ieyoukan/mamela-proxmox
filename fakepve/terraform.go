package fakepve

import "strconv"

// TerraformVMs are VMs Terraform made (no marker of Mamela's), and the JSON of `terraform show
// -json` of the state that made them: two of bpg/proxmox (proxmox_virtual_environment_vm) and two
// of Telmate/proxmox (proxmox_vm_qemu, its 3.x and its 2.x attributes), in pool lab and VMIDs
// 7005..7008 on pve1; one more VM outside the boundary (VMID 120, no pool), a template and a
// container the state also has, and someone else's container. The values are fictional.
//
//   - web (7005, bpg): running, as the state says; a static address.
//   - db (7006, Telmate 3.x): stopped, as the state says (vm_state).
//   - drifted (7007, Telmate 2.x): running, with less memory (1024 MiB) and a smaller disk
//     (12 GiB) than the state says (2048 MiB, 16 GiB): Terraform's apply was not made.
//   - batch (7008, bpg): running, though the state says started = false.
type TerraformVMs struct {
	State []byte
}

// AddTerraformVMs puts the VMs into the fake.
func (s *Server) AddTerraformVMs() TerraformVMs {
	s.mu.Lock()
	defer s.mu.Unlock()
	add := func(v VM) {
		if v.Type == "" {
			v.IDE2 = "local-lvm:vm-" + strconv.FormatInt(v.VMID, 10) + "-cloudinit,media=cdrom"
		}
		s.vms[v.VMID] = &v
	}
	add(VM{VMID: 7005, Name: "web", Node: "pve1", Pool: "lab", Cores: 2, Sockets: 1, Memory: 2048, DiskGiB: 20, Tags: "terraform;web",
		Running: true, Description: "Managed by Terraform.", Net0: "virtio=BC:24:11:00:00:05,bridge=vmbr0,firewall=0"})
	add(VM{VMID: 7006, Name: "db", Node: "pve1", Pool: "lab", Cores: 1, Memory: 1024, DiskGiB: 10, Tags: "terraform",
		Description: "db (Terraform)", Net0: "virtio=BC:24:11:00:00:06,bridge=vmbr0"})
	add(VM{VMID: 7007, Name: "drifted", Node: "pve1", Pool: "lab", Cores: 2, Sockets: 1, Memory: 1024, DiskGiB: 12, Tags: "terraform;legacy-app",
		Running: true, Description: "drifted (Terraform)", Net0: "virtio=BC:24:11:00:00:07,bridge=vmbr0"})
	// Cloned by bpg/proxmox from Mamela's template: the clone carries a copy of the template's
	// marker (as on a real cluster), which does not make it Mamela's.
	add(VM{VMID: 7008, Name: "batch", Node: "pve1", Pool: "lab", Cores: 1, Memory: 1024, DiskGiB: 8, Tags: "terraform",
		Running: true, Net0: "virtio=BC:24:11:00:00:08,bridge=vmbr0",
		Description: "Managed by Mamela (proxmox provider): a template. Do not edit the lines below.\nmamela-name: ubuntu-24.04-pve1\nmamela-image: ubuntu-24.04\n"})
	// Outside the boundary: Terraform's too, and never touched.
	add(VM{VMID: 120, Name: "legacy", Node: "pve1", Cores: 4, Memory: 8192, DiskGiB: 64, Tags: "terraform", Running: true,
		Description: "Managed by Terraform.", Net0: "virtio=BC:24:11:00:01:20,bridge=vmbr0", Uptime: 86400})
	add(VM{VMID: 105, Name: "someone-else", Node: "pve1", Type: "lxc", Running: true, Uptime: 90000})
	return TerraformVMs{State: []byte(terraformState)}
}

const terraformState = `{"format_version":"1.0","terraform_version":"1.9.8","values":{"root_module":{"resources":[
{"address":"proxmox_virtual_environment_vm.web","mode":"managed","type":"proxmox_virtual_environment_vm","name":"web","provider_name":"registry.terraform.io/bpg/proxmox","schema_version":0,"values":{
 "acpi":true,"agent":[{"enabled":true,"timeout":"15m","trim":false,"type":"virtio"}],"bios":"seabios","boot_order":["scsi0"],"clone":[],
 "cpu":[{"affinity":"","architecture":"","cores":2,"flags":[],"hotplugged":0,"limit":0,"numa":false,"sockets":1,"type":"x86-64-v2-AES","units":1024}],
 "description":"Managed by Terraform.",
 "disk":[{"aio":"io_uring","backup":true,"cache":"none","datastore_id":"local-lvm","discard":"on","file_format":"raw","file_id":"","interface":"scsi0","iothread":true,"path_in_datastore":"vm-7005-disk-0","replicate":true,"serial":"","size":20,"speed":[],"ssd":false}],
 "hook_script_file_id":null,"id":"7005",
 "initialization":[{"datastore_id":"local-lvm","dns":[{"domain":"","servers":["192.0.2.1"]}],"interface":"ide2","ip_config":[{"ipv4":[{"address":"192.0.2.50/24","gateway":"192.0.2.1"}],"ipv6":[]}],"user_account":[{"keys":["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample user@example"],"password":"","username":"ubuntu"}],"user_data_file_id":""}],
 "ipv4_addresses":[["127.0.0.1"],["192.0.2.50"]],"mac_addresses":["BC:24:11:00:00:05"],
 "memory":[{"dedicated":2048,"floating":0,"hugepages":"","keep_hugepages":false,"shared":0}],
 "name":"web","network_device":[{"bandwidth":0,"bridge":"vmbr0","disconnected":false,"enabled":true,"firewall":false,"mac_address":"BC:24:11:00:00:05","model":"virtio","mtu":0,"queues":0,"rate_limit":0,"trunks":"","vlan_id":0}],
 "node_name":"pve1","on_boot":true,"operating_system":[{"type":"l26"}],"pool_id":"lab","protection":false,"scsi_hardware":"virtio-scsi-single",
 "started":true,"stop_on_destroy":false,"tags":["terraform","web"],"template":false,"timeout_create":1800,"vm_id":7005}},
{"address":"proxmox_vm_qemu.db","mode":"managed","type":"proxmox_vm_qemu","name":"db","provider_name":"registry.terraform.io/telmate/proxmox","schema_version":0,"values":{
 "additional_wait":5,"agent":1,"bios":"seabios","boot":"order=scsi0","ciuser":"ubuntu","clone":"ubuntu-template","cores":0,
 "cpu":[{"affinity":"","cores":1,"flags":[],"limit":0,"numa":false,"sockets":1,"type":"host","units":0,"vcores":0}],
 "desc":"","description":"db (Terraform)",
 "disk":[],"disks":[{"ide":[{"ide0":[],"ide1":[{"cdrom":[],"cloudinit":[{"storage":"local-lvm"}],"disk":[],"passthrough":[]}],"ide2":[],"ide3":[]}],"sata":[],
  "scsi":[{"scsi0":[{"cdrom":[],"cloudinit":[],"disk":[{"asyncio":"","backup":true,"cache":"","discard":false,"emulatessd":false,"format":"raw","id":0,"linked_disk_id":-1,"readonly":false,"replicate":false,"serial":"","size":"10G","storage":"local-lvm","wwn":""}],"passthrough":[]}],"scsi1":[],"scsi2":[]}],"virtio":[]}],
 "full_clone":true,"hotplug":"network,disk,usb","id":"pve1/qemu/7006","ipconfig0":"ip=dhcp","memory":1024,"name":"db","nameserver":"",
 "network":[{"bridge":"vmbr0","firewall":false,"id":0,"link_down":false,"macaddr":"BC:24:11:00:00:06","model":"virtio","mtu":0,"queues":0,"rate":0,"tag":0}],
 "onboot":false,"os_type":"cloud-init","pool":"lab","qemu_os":"l26","scsihw":"virtio-scsi-single","sockets":0,"tags":"terraform","target_node":"pve1",
 "vm_state":"stopped","vmid":7006}},
{"address":"proxmox_vm_qemu.drifted","mode":"managed","type":"proxmox_vm_qemu","name":"drifted","provider_name":"registry.terraform.io/telmate/proxmox","schema_version":0,"values":{
 "agent":1,"balloon":0,"bios":"seabios","boot":"order=scsi0;net0","clone":"ubuntu-template","cores":2,"cpu":"host","desc":"drifted (Terraform)",
 "disk":[{"aio":"","backup":true,"cache":"","discard":"","file":"vm-7007-disk-0","format":"raw","iothread":0,"mbps":0,"media":"","replicate":0,"size":"16G","slot":0,"ssd":0,"storage":"local-lvm","storage_type":"lvmthin","type":"scsi","volume":"local-lvm:vm-7007-disk-0"}],
 "full_clone":true,"id":"pve1/qemu/7007","ipconfig0":"ip=192.0.2.70/24,gw=192.0.2.1","memory":2048,"name":"drifted","nameserver":"192.0.2.1",
 "network":[{"bridge":"vmbr0","firewall":false,"link_down":false,"macaddr":"BC:24:11:00:00:07","model":"virtio","mtu":0,"queues":0,"rate":0,"tag":-1}],
 "onboot":false,"oncreate":true,"os_type":"cloud-init","pool":"lab","qemu_os":"other","scsihw":"virtio-scsi-pci","sockets":1,"tags":"terraform;legacy-app","target_node":"pve1","vcpus":0,"vmid":7007}},
{"address":"proxmox_virtual_environment_vm.batch","mode":"managed","type":"proxmox_virtual_environment_vm","name":"batch","provider_name":"registry.terraform.io/bpg/proxmox","schema_version":0,"values":{
 "cpu":[{"cores":1,"sockets":1,"type":"x86-64-v2-AES"}],"description":"Managed by Terraform.",
 "disk":[{"datastore_id":"local-lvm","file_format":"raw","interface":"scsi0","size":8}],"id":"7008",
 "initialization":[{"datastore_id":"local-lvm","interface":"ide2","ip_config":[{"ipv4":[{"address":"dhcp","gateway":""}]}]}],
 "memory":[{"dedicated":1024,"floating":0}],"name":"batch","network_device":[{"bridge":"vmbr0","enabled":true,"model":"virtio"}],
 "node_name":"pve1","pool_id":"lab","started":false,"tags":["terraform"],"template":false,"vm_id":7008}},
{"address":"proxmox_virtual_environment_vm.legacy","mode":"managed","type":"proxmox_virtual_environment_vm","name":"legacy","provider_name":"registry.terraform.io/bpg/proxmox","schema_version":0,"values":{
 "cpu":[{"cores":4,"sockets":1}],"description":"Managed by Terraform.","disk":[{"datastore_id":"local-lvm","interface":"scsi0","size":64}],"id":"120",
 "memory":[{"dedicated":8192}],"name":"legacy","network_device":[{"bridge":"vmbr0","model":"virtio"}],"node_name":"pve1","pool_id":"","started":true,
 "tags":["terraform"],"template":false,"vm_id":120}},
{"address":"proxmox_virtual_environment_vm.template","mode":"managed","type":"proxmox_virtual_environment_vm","name":"template","provider_name":"registry.terraform.io/bpg/proxmox","schema_version":0,"values":{
 "cpu":[{"cores":1,"sockets":1}],"disk":[{"datastore_id":"local-lvm","interface":"scsi0","size":4}],"id":"7009","memory":[{"dedicated":512}],
 "name":"ubuntu-template","network_device":[{"bridge":"vmbr0"}],"node_name":"pve1","pool_id":"lab","started":false,"template":true,"vm_id":7009}},
{"address":"proxmox_virtual_environment_container.dns","mode":"managed","type":"proxmox_virtual_environment_container","name":"dns","provider_name":"registry.terraform.io/bpg/proxmox","schema_version":0,"values":{
 "id":"130","node_name":"pve1","vm_id":130}},
{"address":"data.proxmox_virtual_environment_nodes.all","mode":"data","type":"proxmox_virtual_environment_nodes","name":"all","provider_name":"registry.terraform.io/bpg/proxmox","schema_version":0,"values":{"names":["pve1"]}}
]}}}`
