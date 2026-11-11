package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const good = `
[provider.proxmox]
api_url = "https://pve.example:8006/api2/json"
catalog = "builtin:proxmox"
token_id = "mamela@pve!lab"
token_file = "~/.config/mamela/token"
guard_token_id = "mamela-audit@pve!audit"
guard_token_file = "~/.config/mamela/audit-token"

[provider.proxmox.guard]
node = "pve1"
pool = "lab"
vmid_min = 7000
vmid_max = 7009
tag = "mamela-lab"
disk_storage = "local-lvm"
image_storage = "local"
bridge = "vmbr0"
operating_token = "mamela@pve!lab"
max_vms = 4
min_host_mem_available_mib = 4096
max_storage_used_percent = 80
`

func load(t *testing.T, toml string) (Config, error) {
	p := filepath.Join(t.TempDir(), "mamela.providers.toml")
	os.WriteFile(p, []byte(toml), 0o600)
	return Load(p, "proxmox")
}

func TestBootstrap(t *testing.T) {
	c, err := load(t, good)
	if err != nil {
		t.Fatal(err)
	}
	s := Bootstrap(c)
	for _, want := range []string{
		"pveum pool add lab",
		"ensure_role MamelaLabVM",
		"pveum acl modify /pool/lab --tokens 'mamela@pve!lab' --roles MamelaLabVM",
		"pveum acl modify /storage/local-lvm --tokens 'mamela@pve!lab' --roles MamelaLabDisk",
		"pveum acl modify /sdn/zones/localnetwork/vmbr0 --tokens 'mamela@pve!lab' --roles MamelaLabNet",
		"pveum acl modify / --tokens 'mamela-audit@pve!audit' --roles PVEAuditor",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the script lacks %q", want)
		}
	}
	// The operating token never gets anything on /, /vms or /nodes.
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, "'mamela@pve!lab'") && (strings.Contains(l, "modify / ") || strings.Contains(l, " /vms") || strings.Contains(l, " /nodes")) {
			t.Errorf("grants the operating token too much: %s", l)
		}
	}
	if !strings.Contains(Permissions(c), "VM.Allocate") || !strings.Contains(Cleanup(c), "--free") {
		t.Error("permissions or cleanup is incomplete")
	}
}

func TestLoadRejects(t *testing.T) {
	for name, toml := range map[string]string{
		"no guard":          strings.Split(good, "[provider.proxmox.guard]")[0],
		"other token":       strings.Replace(good, `operating_token = "mamela@pve!lab"`, `operating_token = "root@pam!x"`, 1),
		"audit is the user": strings.Replace(good, `"mamela-audit@pve!audit"`, `"mamela@pve!audit"`, 1),
		"no audit token":    strings.Replace(good, `guard_token_id = "mamela-audit@pve!audit"`, ``, 1),
		"unknown setting":   good + "surprise = 1\n",
		"no provider":       strings.Replace(good, "provider.proxmox", "provider.other", 2),
	} {
		if _, err := load(t, toml); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
