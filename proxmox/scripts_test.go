package proxmox

import (
	"context"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/provider/protocol"
)

const goodSettings = `{"node":"pve1","pool":"lab","vmid_min":7000,"vmid_max":7009,"tag":"mamela-lab","disk_storage":"local-lvm",` +
	`"image_storage":"local","bridge":"vmbr0","operating_token":"mamela@pve!lab","max_vms":4,` +
	`"min_host_mem_available_mib":4096,"max_storage_used_percent":80}`

var goodCredentials = protocol.ScriptCredentials{TokenID: "mamela@pve!lab", TokenFile: "~/.config/mamela/token",
	GuardTokenID: "mamela-audit@pve!audit", GuardTokenFile: "~/.config/mamela/audit-token"}

func script(t *testing.T, name string) string {
	t.Helper()
	res, err := Provider{}.Script(context.Background(), protocol.ScriptParams{Name: name, Settings: []byte(goodSettings), Credentials: goodCredentials})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res.Text
}

func TestBootstrap(t *testing.T) {
	s := script(t, "bootstrap")
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
	if !strings.Contains(script(t, "permissions"), "VM.Allocate") || !strings.Contains(script(t, "cleanup"), "--free") {
		t.Error("permissions or cleanup is incomplete")
	}
}

func TestScriptRejects(t *testing.T) {
	for name, edit := range map[string]func(p *protocol.ScriptParams){
		"no guard settings": func(p *protocol.ScriptParams) { p.Settings = nil },
		"other token": func(p *protocol.ScriptParams) {
			p.Settings = []byte(strings.Replace(goodSettings, `"mamela@pve!lab"`, `"root@pam!x"`, 1))
		},
		"audit is the same user": func(p *protocol.ScriptParams) { p.Credentials.GuardTokenID = "mamela@pve!audit" },
		"no audit token":         func(p *protocol.ScriptParams) { p.Credentials.GuardTokenID = "" },
		"no token file":          func(p *protocol.ScriptParams) { p.Credentials.TokenFile = "" },
		"unknown setting": func(p *protocol.ScriptParams) {
			p.Settings = []byte(strings.TrimSuffix(goodSettings, "}") + `,"surprise":1}`)
		},
		"unknown script": func(p *protocol.ScriptParams) { p.Name = "teardown" },
	} {
		p := protocol.ScriptParams{Name: "bootstrap", Settings: []byte(goodSettings), Credentials: goodCredentials}
		edit(&p)
		if _, err := (Provider{}).Script(context.Background(), p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
