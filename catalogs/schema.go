package catalogs

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela-proxmox/proxmox"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// Proxmox VE describes its API in one schema, the apidoc.js of pve-docs: on a node,
// /usr/share/pve-docs/api-viewer/apidoc.js (const apiSchema = [...]). It is not part of this
// repository; pve-api.lock names the release and the sha256 of the one the catalog was last
// checked against, and a new release is checked by giving cmd/proxmox-catalogs its file (README).

// Lock names the schema the catalog was last checked against.
type Lock struct {
	PVEManager string // the pve-manager release the schema is from ("9.2.26")
	SHA256     string // of its apidoc.js
}

//go:embed pve-api.lock
var lockFile []byte

// Locked is the lock of this source (pve-api.lock).
func Locked() (Lock, error) { return ParseLock(lockFile) }

// ParseLock reads a lock: "pve-manager <release>" and "sha256 <hex>" lines, and # comments.
func ParseLock(b []byte) (Lock, error) {
	var l Lock
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, _ := strings.Cut(line, " ")
		switch k {
		case "pve-manager":
			l.PVEManager = strings.TrimSpace(v)
		case "sha256":
			l.SHA256 = strings.TrimSpace(v)
		default:
			return l, fmt.Errorf("pve-api.lock: unknown line %q", line)
		}
	}
	if l.PVEManager == "" || !sha256Re.MatchString(l.SHA256) {
		return l, errors.New("pve-api.lock: a pve-manager release and the sha256 of its apidoc.js are required")
	}
	return l, nil
}

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Encode writes the lock.
func (l Lock) Encode() []byte {
	return fmt.Appendf(nil, "# Proxmox VE's API schema (the apidoc.js of pve-docs) that pome/catalogs/proxmox.json and the permission\n"+
		"# catalog (proxmox/permissions.go) were last checked against: its pve-manager release and sha256.\n"+
		"# Written by go run ./cmd/proxmox-catalogs -schema <apidoc.js> -pve-manager <release> -update-lock (README).\n"+
		"pve-manager %s\nsha256 %s\n", l.PVEManager, l.SHA256)
}

// Schema is Proxmox VE's API schema.
type Schema struct {
	SHA256 string
	paths  []schemaPath
}

type schemaPath struct {
	path    string
	segs    []string
	methods map[string]Method
}

// Method is one method of a path in the schema.
type Method struct {
	Name       string `json:"name"`
	AllowToken int    `json:"allowtoken"`
	Parameters struct {
		Properties map[string]jsontext.Value `json:"properties"`
	} `json:"parameters"`
	Permissions jsontext.Value `json:"permissions"`
}

type node struct {
	Path     string            `json:"path"`
	Info     map[string]Method `json:"info"`
	Children []node            `json:"children"`
}

// ReadSchema reads apidoc.js (or the JSON array it holds).
func ReadSchema(data []byte) (*Schema, error) {
	sum := sha256.Sum256(data)
	s := &Schema{SHA256: hex.EncodeToString(sum[:])}
	start := 0
	if i := bytes.Index(data, []byte("const apiSchema = ")); i >= 0 {
		start = i + len("const apiSchema = ")
	}
	value, err := jsontext.NewDecoder(bytes.NewReader(data[start:])).ReadValue()
	if err != nil {
		return nil, fmt.Errorf("not the apidoc.js of pve-docs (const apiSchema = [...]): %w", err)
	}
	var roots []node
	if err := json.Unmarshal(value, &roots); err != nil {
		return nil, fmt.Errorf("the API schema: %w", err)
	}
	var walk func(ns []node)
	walk = func(ns []node) {
		for _, n := range ns {
			if len(n.Info) > 0 {
				s.paths = append(s.paths, schemaPath{path: n.Path, segs: strings.Split(n.Path, "/"), methods: n.Info})
			}
			walk(n.Children)
		}
	}
	walk(roots)
	if len(s.paths) == 0 {
		return nil, errors.New("the API schema has no paths")
	}
	return s, nil
}

func placeholder(seg string) (string, bool) {
	if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
		return seg[1 : len(seg)-1], true
	}
	return "", false
}

// find is the schema's path a call's path is (the same literal segments, an ID where it has one,
// whatever the IDs are called: the provider's {template} is the schema's {vmid}), and the schema's
// names of the path's IDs.
func (s *Schema) find(path string) (schemaPath, []string, bool) {
	segs := strings.Split(path, "/")
	for _, p := range s.paths {
		if len(p.segs) != len(segs) {
			continue
		}
		var names []string
		match := true
		for i, seg := range segs {
			_, mine := placeholder(seg)
			theirs, isID := placeholder(p.segs[i])
			switch {
			case mine != isID, !mine && seg != p.segs[i]:
				match = false
			case isID:
				names = append(names, theirs)
			}
		}
		if match {
			return p, names, true
		}
	}
	return schemaPath{}, nil, false
}

// has says whether a method of the schema reads a parameter: by its name, or as one of a family
// ("scsi0" of "scsi[n]").
func (m Method) has(param string) bool {
	if _, ok := m.Parameters.Properties[param]; ok {
		return true
	}
	base := strings.TrimRight(param, "0123456789")
	_, ok := m.Parameters.Properties[base+"[n]"]
	return ok && base != param
}

// Validate checks a catalog, and the manifest it was generated from, against the schema: every
// call and parameter exists and takes API tokens; a reviewed call has the handler it was reviewed
// for; what the schema requires of each call the operating token makes is what the permission
// catalog grants (and no more on a VM), and the guard's calls need nothing the audit token
// (PVEAuditor) lacks. It returns every problem.
func Validate(s *Schema, m protocol.Manifest, c protocol.Catalog, reviewed map[string]Review) []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	operating, guard := map[string]protocol.Call{}, map[string]bool{}
	union := func(c protocol.Call) {
		key := c.Method + " " + c.Path
		u := operating[key]
		u.Method, u.Path = c.Method, c.Path
		for _, p := range c.Query {
			if !slices.Contains(u.Query, p) {
				u.Query = append(u.Query, p)
			}
		}
		for _, p := range c.Form {
			if !slices.Contains(u.Form, p) {
				u.Form = append(u.Form, p)
			}
		}
		operating[key] = u
	}
	for _, c := range m.ObserveCalls {
		union(c)
	}
	for _, k := range m.Kinds {
		for _, o := range k.Operations {
			for _, c := range o.Calls {
				union(c)
			}
		}
	}
	if m.Guard != nil {
		for _, c := range m.Guard.Calls {
			guard[c.Method+" "+c.Path] = true
		}
	}
	for _, e := range c.Entries {
		key := e.Method + " " + e.Path
		p, ids, ok := s.find(e.Path)
		if !ok {
			add("%s: no such path in Proxmox VE's API: it was removed or moved; find what replaced it, and change the provider", key)
			continue
		}
		mt, ok := p.methods[e.Method]
		if !ok {
			add("%s: %s has no %s in Proxmox VE's API (it has %s)", key, p.path, e.Method, strings.Join(slices.Sorted(maps.Keys(p.methods)), ", "))
			continue
		}
		if mt.AllowToken != 1 {
			add("%s (%s): Proxmox VE does not allow API tokens to call it", key, mt.Name)
		}
		if r, ok := reviewed[key]; ok && r.Handler != mt.Name {
			add("%s was reviewed for Proxmox VE's handler %q, and the API's is now %q: review the call again (catalogs/review.go)", key, r.Handler, mt.Name)
		}
		params := slices.Concat(e.Query, e.Form)
		if e.File != "" {
			params = append(params, e.File)
		}
		for _, param := range params {
			switch {
			case slices.Contains(ids, param):
				add("%s: %s is an ID of the path in Proxmox VE's API (%s), not a parameter", key, param, p.path)
			case !mt.has(param):
				add("%s (%s): Proxmox VE's API has no parameter %s; it has %s", key, mt.Name, param,
					strings.Join(slices.Sorted(maps.Keys(mt.Parameters.Properties)), ", "))
			}
		}
		check, err := readPermissions(mt.Permissions)
		if err != nil {
			add("%s (%s): its permissions: %v; review them, and teach catalogs/schema.go to read them", key, mt.Name, err)
			continue
		}
		if call, ok := operating[key]; ok {
			privs, known := proxmox.PrivilegesOf(call)
			if !known {
				add("%s: the permission catalog (proxmox/permissions.go) does not know it or a parameter of it", key)
				continue
			}
			granted := map[string]bool{} // "<ref> <priv>": what the bootstrap grants (the first of each)
			for _, pr := range privs {
				granted[pr.Ref+" "+pr.Privs[0]] = true
			}
			if !check.holds(func(path, priv string) bool {
				return slices.ContainsFunc(refsOf(path), func(ref string) bool { return granted[ref+" "+priv] })
			}) {
				add("%s (%s): Proxmox VE requires %s, which the permission catalog (proxmox/permissions.go) does not grant: "+
					"review the call's permissions, change the catalog, and run the bootstrap again", key, mt.Name, mt.Permissions)
			}
			if vms := check.on(proxmox.AtPool); len(vms) > 0 {
				for _, pr := range privs {
					if pr.Ref == proxmox.AtPool && !slices.Contains(vms, pr.Privs[0]) {
						add("%s (%s): the permission catalog grants %s on the pool for it, which Proxmox VE does not check (%s)", key, mt.Name, pr.Privs[0], mt.Permissions)
					}
				}
			}
		}
		if guard[key] && !check.holds(func(_, priv string) bool { return strings.HasSuffix(priv, ".Audit") }) {
			add("%s (%s): the guard reads it with the audit token (PVEAuditor), and Proxmox VE requires %s", key, mt.Name, mt.Permissions)
		}
	}
	return errs
}

// refsOf are the places of the permission catalog (the bootstrap's grants) a path of the schema's
// permissions is: a VM's privileges come from its pool's ACL.
func refsOf(path string) []string {
	switch {
	case path == "/vms" || strings.HasPrefix(path, "/vms/") || strings.HasPrefix(path, "/pool/"):
		return []string{proxmox.AtPool}
	case strings.HasPrefix(path, "/storage/"):
		return []string{proxmox.AtDiskStorage, proxmox.AtImageStorage}
	case strings.HasPrefix(path, "/sdn/"):
		return []string{proxmox.AtBridge}
	}
	return nil
}

// permission is a schema's permission check: a privilege check on a path ("perm": all of privs,
// or one with any), all or one of several ("and", "or"), or none (any user: the handler checks
// what it needs itself, as for creating a VM).
type permission struct {
	op    string // perm, and, or, all (no check)
	path  string
	privs []string
	any   bool
	args  []permission
}

func readPermissions(raw jsontext.Value) (permission, error) {
	var p struct {
		Check jsontext.Value `json:"check"`
		User  string         `json:"user"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return permission{}, err
	}
	if len(p.Check) == 0 {
		if p.User == "all" {
			return permission{op: "all"}, nil
		}
		return permission{}, fmt.Errorf("no check, and not open to all users (%s)", raw)
	}
	return readCheck(p.Check)
}

func readCheck(raw jsontext.Value) (permission, error) {
	var parts []jsontext.Value
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) == 0 {
		return permission{}, fmt.Errorf("check %s", raw)
	}
	var op string
	json.Unmarshal(parts[0], &op)
	switch op {
	case "perm":
		p := permission{op: op}
		if len(parts) < 3 || json.Unmarshal(parts[1], &p.path) != nil || json.Unmarshal(parts[2], &p.privs) != nil {
			return permission{}, fmt.Errorf("check %s", raw)
		}
		for i := 3; i+1 < len(parts); i += 2 {
			var k string
			json.Unmarshal(parts[i], &k)
			switch k {
			case "any":
				p.any = string(parts[i+1]) == "1"
			case "require_param":
			default:
				return permission{}, fmt.Errorf("check %s: option %q", raw, k)
			}
		}
		return p, nil
	case "and", "or":
		p := permission{op: op}
		for _, a := range parts[1:] {
			x, err := readCheck(a)
			if err != nil {
				return permission{}, err
			}
			p.args = append(p.args, x)
		}
		return p, nil
	}
	return permission{}, fmt.Errorf("check %q is not one of perm, and, or", op)
}

// holds says whether the check holds for a holder of the privileges has says.
func (p permission) holds(has func(path, priv string) bool) bool {
	switch p.op {
	case "all":
		return true
	case "perm":
		n := 0
		for _, priv := range p.privs {
			if has(p.path, priv) {
				n++
			}
		}
		return n > 0 && (p.any || n == len(p.privs))
	case "and":
		for _, a := range p.args {
			if !a.holds(has) {
				return false
			}
		}
		return true
	case "or":
		return slices.ContainsFunc(p.args, func(a permission) bool { return a.holds(has) })
	}
	return false
}

// on are the privileges the check names on paths of a place of the permission catalog.
func (p permission) on(ref string) []string {
	var out []string
	if p.op == "perm" && slices.Contains(refsOf(p.path), ref) {
		out = append(out, p.privs...)
	}
	for _, a := range p.args {
		out = append(out, a.on(ref)...)
	}
	return out
}
