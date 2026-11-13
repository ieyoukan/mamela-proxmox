package proxmox

import (
	"fmt"
	"strings"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// The scripts (Mamela's ADR 0006) a person reviews and runs as root on a Proxmox node to set up and
// tidy the boundary this provider works in: Mamela's tokens can never do these things themselves.
// The provider writes them from the settings alone (mamela provider script <pome-dir> proxmox
// <script>), so they come from the same pinned program as the grants the guard checks.

// Scripts are the scripts the provider writes.
var Scripts = []protocol.Script{
	{Name: "bootstrap", Description: "run as root on a Proxmox node: create the pool, the roles, the operating and audit users and tokens, and exactly the grants (removing grants no longer needed)"},
	{Name: "permissions", Description: "explain every privilege of the operating token and the operations that need it (text, not a script)"},
	{Name: "cleanup", Description: "run as root on a Proxmox node: list the uploads (mamela-*) no VM uses; remove them only with --free"},
}

// ScriptConfig is what the scripts need: the guard's settings and the tokens.
type ScriptConfig struct {
	Guard          GuardSettings
	TokenID        string // the operating token
	TokenFile      string
	GuardTokenID   string // the audit token
	GuardTokenFile string
}

// NewScriptConfig checks the parameters of a script.
func NewScriptConfig(p protocol.ScriptParams) (ScriptConfig, error) {
	g, err := ParseGuardSettings(p.Settings)
	if err != nil {
		return ScriptConfig{}, err
	}
	cr := p.Credentials
	c := ScriptConfig{Guard: g, TokenID: cr.TokenID, TokenFile: cr.TokenFile, GuardTokenID: cr.GuardTokenID, GuardTokenFile: cr.GuardTokenFile}
	switch {
	case c.TokenID != g.OperatingToken:
		return ScriptConfig{}, fmt.Errorf("[provider.proxmox]: token_id %q and guard.operating_token %q must be the same token", c.TokenID, g.OperatingToken)
	case c.TokenFile == "":
		return ScriptConfig{}, fmt.Errorf("[provider.proxmox]: token_file is required")
	case !strings.Contains(c.GuardTokenID, "!") || c.GuardTokenFile == "":
		return ScriptConfig{}, fmt.Errorf("[provider.proxmox]: guard_token_id (user@realm!name) and guard_token_file are required: the guard's read-only audit token")
	case user(c.GuardTokenID) == user(c.TokenID):
		return ScriptConfig{}, fmt.Errorf("[provider.proxmox]: the audit token must belong to another user than the operating token (it may see the whole cluster)")
	}
	return c, nil
}

// writeScript writes a script (kit.Provider.ScriptFunc), with the provider's manifest.
func writeScript(m protocol.Manifest, p protocol.ScriptParams) (protocol.ScriptResult, error) {
	c, err := NewScriptConfig(p)
	if err != nil {
		return protocol.ScriptResult{}, err
	}
	switch p.Name {
	case "bootstrap":
		return protocol.ScriptResult{Text: Bootstrap(m, c)}, nil
	case "permissions":
		return protocol.ScriptResult{Text: Permissions(m, c)}, nil
	case "cleanup":
		return protocol.ScriptResult{Text: Cleanup(c)}, nil
	}
	return protocol.ScriptResult{}, fmt.Errorf("no script %q", p.Name)
}

func user(token string) string { u, _, _ := strings.Cut(token, "!"); return u }

func tokenName(token string) string { _, n, _ := strings.Cut(token, "!"); return n }

// role is the role of a grant's path: one per path, so a privilege never reaches a path that does
// not need it. The names are those of the earlier lab bootstrap, so that running this one on a
// lab set up by it converges.
func role(g GuardSettings, path string) string {
	switch path {
	case "/pool/" + g.Pool:
		return "MamelaLabVM"
	case "/storage/" + g.DiskStorage:
		return "MamelaLabDisk"
	case "/storage/" + g.ImageStorage:
		return "MamelaLabImage"
	}
	if strings.HasPrefix(path, "/sdn/") {
		return "MamelaLabNet"
	}
	return "MamelaLab" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, path)
}

// Permissions explains every grant: the path, the role, each privilege and the operations that
// need it.
func Permissions(m protocol.Manifest, c ScriptConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The operating token %s gets exactly these privileges (from the provider's manifest):\n", c.TokenID)
	for _, g := range Grants(m, c.Guard) {
		fmt.Fprintf(&b, "\n%s (role %s)\n", g.Path, role(c.Guard, g.Path))
		for _, p := range g.Privs {
			fmt.Fprintf(&b, "  %-26s %s\n", p, strings.Join(g.Reasons[p], ", "))
		}
	}
	fmt.Fprintf(&b, "\nA pool's privileges reach the VMs in it (/vms/%d..%d). The guard blocks an apply when the token holds\n"+
		"anything else, or lacks any of these. The audit token %s gets PVEAuditor (read-only) on /.\n", c.Guard.VMIDMin, c.Guard.VMIDMax, c.GuardTokenID)
	return b.String()
}

// Bootstrap is the setup script: the pool, the import content type on the image storage, one role
// per path, the operating user and token with exactly the grants, the removal of their grants that
// are no longer needed, and the audit user and token. Running it again converges.
func Bootstrap(m protocol.Manifest, c ScriptConfig) string {
	g := c.Guard
	grants := Grants(m, g)
	opUser, auditUser := user(c.TokenID), user(c.GuardTokenID)
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	w("#!/usr/bin/env bash")
	w("# Mamela Proxmox bootstrap — written by the proxmox provider (mamela provider script <pome-dir> proxmox bootstrap). Review every line before running it.")
	w("# Run as root on one node of the cluster. It adds a pool, roles, users, tokens and ACLs, and removes ACL")
	w("# entries of %s and %s that the provider no longer needs. It touches no guest, no other", opUser, c.TokenID)
	w("# user's permissions, and no storage, network or HA setting except step 2.")
	w("set -euo pipefail")
	w("")
	w("exists() { pvesh get \"$1\" --output-format json >/dev/null 2>&1; }")
	w("ensure_role() { if exists \"/access/roles/$1\"; then pveum role modify \"$1\" --privs \"$2\"; else pveum role add \"$1\" --privs \"$2\"; fi; }")
	w("ensure_user() { exists \"/access/users/$1\" || pveum user add \"$1\" --comment \"$2\"; }")
	w("new_token() { # prints the secret once; store it in the given file with mode 600")
	w("  if exists \"/access/users/$1/token/$2\"; then echo \"token $1!$2 already exists (secret not shown again)\"; return; fi")
	w("  echo \"== token $1!$2 — save the 'value' below into $3 (chmod 600) on the machine that runs Mamela\"")
	w("  pveum user token add \"$1\" \"$2\" --privsep 1 --comment \"$4\"")
	w("}")
	w("")
	w("# 1. The pool. The provider creates VMs only inside it.")
	w("exists \"/pools/%s\" || pveum pool add %s --comment \"Mamela: guests here are managed by Mamela\"", g.Pool, g.Pool)
	w("")
	w("# 2. Let the image storage hold importable disk images (content type \"import\"). This only adds")
	w("#    \"import\" to the storage's content types; the existing ones are kept.")
	w("content=\"$(pvesh get /storage/%s --output-format json | python3 -c 'import json,sys; print(json.load(sys.stdin)[\"content\"])')\"", g.ImageStorage)
	w("case \",$content,\" in *,import,*) ;; *) pvesm set %s --content \"$content,import\" ;; esac", g.ImageStorage)
	w("")
	w("# 3. One role per path, with only the privileges that path needs (mamela provider script <pome-dir> proxmox permissions says why).")
	for _, gr := range grants {
		w("ensure_role %s %q", role(g, gr.Path), strings.Join(gr.Privs, ","))
	}
	w("")
	w("# 4. The operating user and its privilege-separated token (no password, no login).")
	w("ensure_user %s \"Mamela Proxmox provider\"", opUser)
	w("new_token %s %s %q \"Mamela: changes inside pool %s only\"", opUser, tokenName(c.TokenID), c.TokenFile, g.Pool)
	w("")
	w("# 5. ACLs. With privilege separation, the token gets the intersection of its own ACLs and its user's,")
	w("#    so both receive the same grants. Nothing is granted on /, /vms or other pools.")
	var keep []string
	for _, gr := range grants {
		w("pveum acl modify %s --users %s --roles %s", gr.Path, opUser, role(g, gr.Path))
		w("pveum acl modify %s --tokens '%s' --roles %s", gr.Path, c.TokenID, role(g, gr.Path))
		keep = append(keep, gr.Path+" "+role(g, gr.Path))
	}
	w("")
	w("# 6. Remove the operating user's and token's ACL entries that are not in step 5 (left by earlier")
	w("#    versions of this script). Only these two identities are looked at.")
	w("keep=%q", strings.Join(keep, "\n"))
	w("pvesh get /access/acl --output-format json | python3 -c '%s' \"$keep\" '%s' '%s' |", staleACLs, opUser, c.TokenID)
	w("while read -r type ugid path role; do")
	w("  echo \"removing role $role on $path from $ugid (the provider no longer needs it)\"")
	w("  if [ \"$type\" = token ]; then pveum acl delete \"$path\" --tokens \"$ugid\" --roles \"$role\"; else pveum acl delete \"$path\" --users \"$ugid\" --roles \"$role\"; fi")
	w("done")
	w("")
	w("# 7. The audit token: read-only everywhere (built-in PVEAuditor). Only the guard uses it, to check")
	w("#    the cluster and that guests outside the boundary are unchanged.")
	w("ensure_user %s \"Mamela audit (read-only)\"", auditUser)
	w("new_token %s %s %q \"Mamela: read-only audit\"", auditUser, tokenName(c.GuardTokenID), c.GuardTokenFile)
	w("pveum acl modify / --users %s --roles PVEAuditor", auditUser)
	w("pveum acl modify / --tokens '%s' --roles PVEAuditor", c.GuardTokenID)
	w("")
	w("echo \"done. next: mamela guard <pome-dir> (the guard's check before an apply)\"")
	return b.String()
}

// staleACLs prints "type ugid path role" for every ACL entry of the given user or token (argv 2, 3)
// whose (path, role) is not one of the lines in argv 1.
const staleACLs = `import json,sys
keep=set(tuple(l.split(" ",1)) for l in sys.argv[1].split("\\n") if l)
for a in json.load(sys.stdin):
    if a["ugid"] in (sys.argv[2], sys.argv[3]) and (a["path"], a["roleid"]) not in keep:
        print(a["type"], a["ugid"], a["path"], a["roleid"])`

// Cleanup is a script that finds the files the provider uploaded to the image storage (images and
// seeds, named mamela-...) that no VM of the cluster uses, lists them, and removes them only when
// run with --free. The provider's token cannot remove files: that would need Datastore.Allocate on
// the whole storage, which also holds other guests' backups and templates.
func Cleanup(c ScriptConfig) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	w("#!/usr/bin/env bash")
	w("# Mamela Proxmox cleanup — written by the proxmox provider (mamela provider script <pome-dir> proxmox cleanup). Review it before running it as root.")
	w("# It lists the files on storage %s whose names start with mamela- and that no VM of the cluster", c.Guard.ImageStorage)
	w("# refers to. It removes them only when run with --free.")
	w("set -euo pipefail")
	w("unused=\"$(python3 -c '%s' %s %s)\"", unusedVolumes, c.Guard.Node, c.Guard.ImageStorage)
	w("if [ -z \"$unused\" ]; then echo 'nothing to remove'; exit 0; fi")
	w("echo \"unused uploads:\"; echo \"$unused\"")
	w("if [ \"${1:-}\" != --free ]; then echo 'run again with --free to remove them'; exit 0; fi")
	w("echo \"$unused\" | while read -r vol; do pvesm free \"$vol\"; done")
	return b.String()
}

// unusedVolumes prints the mamela-* volumes of storage argv 2 (as seen from node argv 1) that no
// VM's configuration refers to, on any node.
const unusedVolumes = `import json,subprocess,sys
def get(p, *a): return json.loads(subprocess.check_output(["pvesh","get",p,"--output-format","json",*a]))
used=set()
for r in get("/cluster/resources","--type","vm"):
    if r.get("type")!="qemu": continue
    for v in get("/nodes/%s/qemu/%d/config"%(r["node"],r["vmid"])).values():
        for part in str(v).split(","):
            used.add(part.split("import-from=")[-1])
for content in ("iso","import"):
    for v in get("/nodes/%s/storage/%s/content"%(sys.argv[1],sys.argv[2]),"--content",content):
        if v["volid"].split("/",1)[-1].startswith("mamela-") and v["volid"] not in used: print(v["volid"])`
