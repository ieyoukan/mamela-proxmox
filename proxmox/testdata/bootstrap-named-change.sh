#!/usr/bin/env bash
# Mamela Proxmox bootstrap — written by the proxmox provider (mamela provider script <pome-dir> proxmox bootstrap). Review every line before running it.
# Run as root on one node of the cluster. It adds a pool, roles, users, tokens and ACLs, and removes ACL
# entries of mamela@pve and mamela@pve!lab that the provider no longer needs. It touches no guest, no other
# user's permissions, and no storage, network or HA setting except step 2.
set -euo pipefail

exists() { pvesh get "$1" --output-format json >/dev/null 2>&1; }
ensure_role() { if exists "/access/roles/$1"; then pveum role modify "$1" --privs "$2"; else pveum role add "$1" --privs "$2"; fi; }
ensure_user() { exists "/access/users/$1" || pveum user add "$1" --comment "$2"; }
new_token() { # prints the secret once; store it in the given file with mode 600
  if exists "/access/users/$1/token/$2"; then echo "token $1!$2 already exists (secret not shown again)"; return; fi
  echo "== token $1!$2 — save the 'value' below into $3 (chmod 600) on the machine that runs Mamela"
  pveum user token add "$1" "$2" --privsep 1 --comment "$4"
}

# 1. The pool. The provider creates VMs only inside it.
exists "/pools/lab" || pveum pool add lab --comment "Mamela: guests here are managed by Mamela"

# 2. Let the image storage hold importable disk images (content type "import"). This only adds
#    "import" to the storage's content types; the existing ones are kept.
content="$(pvesh get /storage/local --output-format json | python3 -c 'import json,sys; print(json.load(sys.stdin)["content"])')"
case ",$content," in *,import,*) ;; *) pvesm set local --content "$content,import" ;; esac

# 3. One role per path, with only the privileges that path needs (mamela provider script <pome-dir> proxmox permissions says why);
#    for named guests, one role per step of their access.
ensure_role MamelaLabVM "Pool.Audit,VM.Allocate,VM.Audit,VM.Clone,VM.Config.CDROM,VM.Config.CPU,VM.Config.Disk,VM.Config.HWType,VM.Config.Memory,VM.Config.Network,VM.Config.Options,VM.PowerMgmt"
ensure_role MamelaLabNet "SDN.Use"
ensure_role MamelaLabImage "Datastore.AllocateTemplate,Datastore.Audit"
ensure_role MamelaLabDisk "Datastore.AllocateSpace"
ensure_role MamelaLabNamedChange "VM.Audit,VM.Config.CPU,VM.Config.Disk,VM.Config.Memory,VM.Config.Options,VM.PowerMgmt"

# 4. The operating user and its privilege-separated token (no password, no login).
ensure_user mamela@pve "Mamela Proxmox provider"
new_token mamela@pve lab "~/.config/mamela/token" "Mamela: changes inside pool lab only"

# 5. ACLs. With privilege separation, the token gets the intersection of its own ACLs and its user's,
#    so both receive the same grants. Nothing is granted on /, /vms or other pools.
pveum acl modify /pool/lab --users mamela@pve --roles MamelaLabVM
pveum acl modify /pool/lab --tokens 'mamela@pve!lab' --roles MamelaLabVM
pveum acl modify /sdn/zones/localnetwork/vmbr0 --users mamela@pve --roles MamelaLabNet
pveum acl modify /sdn/zones/localnetwork/vmbr0 --tokens 'mamela@pve!lab' --roles MamelaLabNet
pveum acl modify /storage/local --users mamela@pve --roles MamelaLabImage
pveum acl modify /storage/local --tokens 'mamela@pve!lab' --roles MamelaLabImage
pveum acl modify /storage/local-lvm --users mamela@pve --roles MamelaLabDisk
pveum acl modify /storage/local-lvm --tokens 'mamela@pve!lab' --roles MamelaLabDisk

# 5b. Existing guests named in [provider.proxmox.guard], outside pool lab and VMIDs 7000..7009: each on its own
#     /vms/<vmid> only. It stays where it is (it is not put in the pool). Its access is a person's decision:
#       access = "observe": VM.Audit only (Mamela sees it, plans and previews; it changes nothing);
#       access = "change": also what adopting and changing it need (mamela provider script <pome-dir> proxmox permissions).
#     To let Mamela adopt and change a guest, set access = "change" in its entry and run this script again;
#     back to "observe", or removing the entry, takes the grant away (step 6).
# guest 150 (lxc on pve2), access change: VM.Audit, VM.Config.CPU, VM.Config.Disk, VM.Config.Memory, VM.Config.Options, VM.PowerMgmt
pveum acl modify /vms/150 --users mamela@pve --roles MamelaLabNamedChange
pveum acl modify /vms/150 --tokens 'mamela@pve!lab' --roles MamelaLabNamedChange

# 6. Remove the operating user's and token's ACL entries that are not in step 5 (left by earlier
#    versions of this script). Only these two identities are looked at.
keep="/pool/lab MamelaLabVM\n/sdn/zones/localnetwork/vmbr0 MamelaLabNet\n/storage/local MamelaLabImage\n/storage/local-lvm MamelaLabDisk\n/vms/150 MamelaLabNamedChange"
pvesh get /access/acl --output-format json | python3 -c 'import json,sys
keep=set(tuple(l.split(" ",1)) for l in sys.argv[1].split("\\n") if l)
for a in json.load(sys.stdin):
    if a["ugid"] in (sys.argv[2], sys.argv[3]) and (a["path"], a["roleid"]) not in keep:
        print(a["type"], a["ugid"], a["path"], a["roleid"])' "$keep" 'mamela@pve' 'mamela@pve!lab' |
while read -r type ugid path role; do
  echo "removing role $role on $path from $ugid (the provider no longer needs it)"
  if [ "$type" = token ]; then pveum acl delete "$path" --tokens "$ugid" --roles "$role"; else pveum acl delete "$path" --users "$ugid" --roles "$role"; fi
done

# 7. The audit token: read-only everywhere (built-in PVEAuditor). Only the guard uses it, to check
#    the cluster and that guests outside the boundary are unchanged.
ensure_user mamela-audit@pve "Mamela audit (read-only)"
new_token mamela-audit@pve audit "~/.config/mamela/audit-token" "Mamela: read-only audit"
pveum acl modify / --users mamela-audit@pve --roles PVEAuditor
pveum acl modify / --tokens 'mamela-audit@pve!audit' --roles PVEAuditor

echo "done. next: mamela guard <pome-dir> (the guard's check before an apply)"
