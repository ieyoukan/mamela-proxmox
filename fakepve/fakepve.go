// Package fakepve is a fake of the Proxmox VE API calls the provider uses, for its tests: VMs and
// storage contents in memory, tasks that end at once, and configuration digests that refuse a
// change based on an older read (HTTP 500, as Proxmox does).
package fakepve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// VM is one fake VM.
type VM struct {
	VMID        int64
	Name, Node  string
	Pool        string
	Description string
	Cores       int64
	Memory      int64
	DiskGiB     int64
	Tags        string
	Running     bool
	Type        string            // qemu (when empty) or lxc: a container is someone else's guest
	Uptime      int64             // seconds, while running (120 when 0)
	Config      map[string]string // other parameters, as created
	Serial      int               // bumped on every change; the digest is derived from it
	Template    bool              // converted to a template
	Base        int64             // the template it is a linked clone of
	IDE2        string            // the seed's drive
}

func (v VM) digest() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%d", v.VMID, v.Serial)))
	return hex.EncodeToString(sum[:20])
}

// Server is the fake API.
type Server struct {
	mu      sync.Mutex
	vms     map[int64]*VM
	volumes map[string]string // volid -> content
	// Uploads counts the uploads it received.
	Uploads int
	// Node is the one node of the fake cluster ("pve1"); Permissions are what /access/permissions
	// answers for any token; MemAvailable (bytes) and DiskTotal, DiskUsed (bytes, of every storage)
	// what the node's and storages' status say; ImportStorages the storages that accept images.
	Node                              string
	Permissions                       map[string]map[string]int
	MemAvailable, DiskTotal, DiskUsed int64
	ImportStorages                    []string
}

// New returns a fake with no VMs and empty storages, on node pve1 with 64 GiB of memory available
// and storages of 1 TiB, a tenth used, of which "local" accepts images to import.
func New() *Server {
	return &Server{vms: map[int64]*VM{}, volumes: map[string]string{}, Node: "pve1",
		MemAvailable: 64 << 30, DiskTotal: 1 << 40, DiskUsed: 100 << 30, ImportStorages: []string{"local"}}
}

// Put stores a VM as is.
func (s *Server) Put(v VM) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vms[v.VMID] = &v
}

// VMs returns a copy of the VMs.
func (s *Server) VMs() map[int64]VM {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int64]VM{}
	for k, v := range s.vms {
		out[k] = *v
	}
	return out
}

// Volumes returns the volumes on the storages.
func (s *Server) Volumes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.volumes))
}

func reply(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.MarshalWrite(w, map[string]any{"data": data}, json.Deterministic(true))
}

// diskOption writes the size as Proxmox does: in MiB after an import (3.5 GiB), in GiB once resized.
func diskOption(v *VM) string {
	switch {
	case v.Template:
		return fmt.Sprintf("local-lvm:base-%d-disk-0,discard=on,size=3584M", v.VMID)
	case v.Base != 0 && v.DiskGiB == 3:
		return fmt.Sprintf("local-lvm:base-%d-disk-0/vm-%d-disk-0,discard=on,size=3584M", v.Base, v.VMID)
	case v.Base != 0:
		return fmt.Sprintf("local-lvm:base-%d-disk-0/vm-%d-disk-0,discard=on,size=%dG", v.Base, v.VMID, v.DiskGiB)
	}
	if v.DiskGiB == 3 {
		return fmt.Sprintf("local-lvm:vm-%d-disk-0,discard=on,size=3584M", v.VMID)
	}
	return fmt.Sprintf("local-lvm:vm-%d-disk-0,discard=on,size=%dG", v.VMID, v.DiskGiB)
}

func upid(node, kind string, vmid int64) string {
	return fmt.Sprintf("UPID:%s:0000AAAA:0000BBBB:00000000:%s:%d:fake@pve!t:", node, kind, vmid)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cluster/resources", func(w http.ResponseWriter, r *http.Request) {
		out := []map[string]any{}
		for _, id := range slices.Sorted(maps.Keys(s.vms)) {
			v := s.vms[id]
			status, uptime, typ := "stopped", int64(0), "qemu"
			if v.Running {
				status, uptime = "running", 120
				if v.Uptime != 0 {
					uptime = v.Uptime
				}
			}
			if v.Type != "" {
				typ = v.Type
			}
			out = append(out, map[string]any{"vmid": v.VMID, "name": v.Name, "node": v.Node, "pool": v.Pool, "type": typ,
				"status": status, "uptime": uptime})
		}
		reply(w, 200, out)
	})
	mux.HandleFunc("GET /cluster/status", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, []map[string]any{{"type": "cluster", "name": "fake", "quorate": 1, "nodes": 1},
			{"type": "node", "name": s.Node, "online": 1}})
	})
	mux.HandleFunc("GET /nodes/{node}/status", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"memory": map[string]any{"available": s.MemAvailable, "free": s.MemAvailable, "total": 128 << 30}})
	})
	mux.HandleFunc("GET /nodes/{node}/storage/{storage}/status", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"total": s.DiskTotal, "used": s.DiskUsed})
	})
	mux.HandleFunc("GET /nodes/{node}/storage", func(w http.ResponseWriter, r *http.Request) {
		out := []map[string]any{}
		for _, st := range s.ImportStorages {
			out = append(out, map[string]any{"storage": st})
		}
		reply(w, 200, out)
	})
	mux.HandleFunc("GET /access/permissions", func(w http.ResponseWriter, r *http.Request) {
		perms := s.Permissions
		if perms == nil {
			perms = map[string]map[string]int{}
		}
		reply(w, 200, perms)
	})
	mux.HandleFunc("GET /nodes/{node}/lxc/{vmid}/config", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("vmid"), 10, 64)
		if v, ok := s.vms[id]; ok && v.Type == "lxc" {
			reply(w, 200, map[string]any{"digest": v.digest(), "hostname": v.Name})
			return
		}
		reply(w, 500, nil)
	})
	vm := func(w http.ResponseWriter, r *http.Request) *VM {
		id, _ := strconv.ParseInt(r.PathValue("vmid"), 10, 64)
		v, ok := s.vms[id]
		if !ok || v.Node != r.PathValue("node") || v.Type == "lxc" {
			reply(w, 500, nil)
			return nil
		}
		return v
	}
	mux.HandleFunc("GET /nodes/{node}/qemu/{vmid}/config", func(w http.ResponseWriter, r *http.Request) {
		if v := vm(w, r); v != nil {
			cfg := map[string]any{"name": v.Name, "cores": v.Cores, "memory": strconv.FormatInt(v.Memory, 10),
				"description": v.Description, "digest": v.digest(), "scsi0": diskOption(v)}
			if v.Tags != "" {
				cfg["tags"] = v.Tags
			}
			if v.Template {
				cfg["template"] = 1
			}
			if v.IDE2 != "" {
				cfg["ide2"] = v.IDE2
			}
			reply(w, 200, cfg)
		}
	})
	mux.HandleFunc("GET /nodes/{node}/storage/{storage}/content", func(w http.ResponseWriter, r *http.Request) {
		out := []map[string]any{}
		prefix := r.PathValue("storage") + ":" + r.URL.Query().Get("content") + "/"
		for _, id := range slices.Sorted(maps.Keys(s.volumes)) {
			if strings.HasPrefix(id, prefix) {
				out = append(out, map[string]any{"volid": id})
			}
		}
		reply(w, 200, out)
	})
	mux.HandleFunc("POST /nodes/{node}/storage/{storage}/upload", func(w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			reply(w, 400, "not multipart")
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		form := map[string]string{}
		var name, content string
		var order []string
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			order = append(order, p.FormName())
			if p.FormName() == "filename" {
				name, content = p.FileName(), string(b)
			} else {
				form[p.FormName()] = string(b)
			}
		}
		// pveproxy reads these fields in this order, then the file; anything else is a bad request.
		if strings.Join(order, ",") != "content,checksum-algorithm,checksum,filename" {
			reply(w, 400, "fields out of order: "+strings.Join(order, ","))
			return
		}
		sum := sha256.Sum256([]byte(content))
		if name == "" || form["checksum-algorithm"] != "sha256" || (form["checksum"] != hex.EncodeToString(sum[:]) && !strings.HasPrefix(content, "fake ")) {
			reply(w, 400, "checksum mismatch or no file")
			return
		}
		s.volumes[r.PathValue("storage")+":"+form["content"]+"/"+name] = content
		s.Uploads++
		reply(w, 200, upid(r.PathValue("node"), "imgcopy", 0))
	})
	mux.HandleFunc("POST /nodes/{node}/qemu", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		id, _ := strconv.ParseInt(r.PostForm.Get("vmid"), 10, 64)
		if _, exists := s.vms[id]; exists || id == 0 {
			reply(w, 500, "VM already exists")
			return
		}
		// The disk is imported from an uploaded image, the seed attached from an uploaded ISO.
		_, from, _ := strings.Cut(r.PostForm.Get("scsi0"), "import-from=")
		from, _, _ = strings.Cut(from, ",")
		seed, _, _ := strings.Cut(r.PostForm.Get("ide2"), ",")
		if _, ok := s.volumes[from]; !ok {
			reply(w, 500, "no image "+from)
			return
		}
		if _, ok := s.volumes[seed]; !ok && seed != "" {
			reply(w, 500, "no seed "+seed)
			return
		}
		cores, _ := strconv.ParseInt(r.PostForm.Get("cores"), 10, 64)
		mem, _ := strconv.ParseInt(r.PostForm.Get("memory"), 10, 64)
		cfg := map[string]string{}
		for k := range r.PostForm {
			cfg[k] = r.PostForm.Get(k)
		}
		s.vms[id] = &VM{VMID: id, Name: r.PostForm.Get("name"), Node: r.PathValue("node"), Pool: r.PostForm.Get("pool"),
			Description: r.PostForm.Get("description"), Cores: cores, Memory: mem, DiskGiB: 3, Config: cfg, IDE2: r.PostForm.Get("ide2")}
		reply(w, 200, upid(r.PathValue("node"), "qmcreate", id))
	})
	// A template: a stopped VM converted once; a linked clone: a new VM on its base disk.
	mux.HandleFunc("POST /nodes/{node}/qemu/{vmid}/template", func(w http.ResponseWriter, r *http.Request) {
		if v := vm(w, r); v != nil {
			if v.Running {
				reply(w, 500, "you can't convert a running VM to a template")
				return
			}
			v.Template = true
			v.Serial++
			reply(w, 200, nil)
		}
	})
	mux.HandleFunc("POST /nodes/{node}/qemu/{vmid}/clone", func(w http.ResponseWriter, r *http.Request) {
		src := vm(w, r)
		if src == nil {
			return
		}
		r.ParseForm()
		id, _ := strconv.ParseInt(r.PostForm.Get("newid"), 10, 64)
		switch {
		case !src.Template || r.PostForm.Get("full") != "0":
			reply(w, 500, "linked clone feature is only available for templates")
			return
		case id == 0:
			reply(w, 400, "newid is required")
			return
		}
		if _, exists := s.vms[id]; exists {
			reply(w, 500, "VM already exists")
			return
		}
		s.vms[id] = &VM{VMID: id, Name: r.PostForm.Get("name"), Node: src.Node, Pool: r.PostForm.Get("pool"),
			Description: r.PostForm.Get("description"), Cores: src.Cores, Memory: src.Memory, DiskGiB: 3, Base: src.VMID,
			Config: map[string]string{"net0": src.Config["net0"]}}
		reply(w, 200, upid(src.Node, "qmclone", src.VMID))
	})
	changed := func(w http.ResponseWriter, r *http.Request, v *VM) bool {
		r.ParseForm()
		if d := r.PostForm.Get("digest"); d != "" && d != v.digest() {
			reply(w, 500, "detected modified configuration")
			return false
		}
		return true
	}
	mux.HandleFunc("PUT /nodes/{node}/qemu/{vmid}/resize", func(w http.ResponseWriter, r *http.Request) {
		v := vm(w, r)
		if v == nil || !changed(w, r, v) {
			return
		}
		n, _ := strconv.ParseInt(strings.TrimSuffix(r.PostForm.Get("size"), "G"), 10, 64)
		if r.PostForm.Get("disk") != "scsi0" || n < v.DiskGiB {
			reply(w, 500, "shrinking disks is not supported")
			return
		}
		v.DiskGiB = n
		v.Serial++
		reply(w, 200, upid(v.Node, "resize", v.VMID))
	})
	mux.HandleFunc("PUT /nodes/{node}/qemu/{vmid}/config", func(w http.ResponseWriter, r *http.Request) {
		v := vm(w, r)
		if v == nil || !changed(w, r, v) {
			return
		}
		if c := r.PostForm.Get("cores"); c != "" {
			v.Cores, _ = strconv.ParseInt(c, 10, 64)
		}
		if m := r.PostForm.Get("memory"); m != "" {
			v.Memory, _ = strconv.ParseInt(m, 10, 64)
		}
		if t := r.PostForm.Get("tags"); t != "" {
			v.Tags = t
		}
		if d, ok := r.PostForm["description"]; ok {
			v.Description = d[0]
		}
		if d := r.PostForm.Get("ide2"); d != "" {
			seed, _, _ := strings.Cut(d, ",")
			if _, ok := s.volumes[seed]; !ok {
				reply(w, 500, "no seed "+seed)
				return
			}
			v.IDE2 = d
		}
		v.Serial++
		reply(w, 200, nil)
	})
	mux.HandleFunc("POST /nodes/{node}/qemu/{vmid}/status/start", func(w http.ResponseWriter, r *http.Request) {
		if v := vm(w, r); v != nil {
			if v.Template {
				reply(w, 500, "you can't start a vm if it's a template")
				return
			}
			v.Running = true
			reply(w, 200, upid(v.Node, "qmstart", v.VMID))
		}
	})
	mux.HandleFunc("POST /nodes/{node}/qemu/{vmid}/status/reboot", func(w http.ResponseWriter, r *http.Request) {
		if v := vm(w, r); v != nil {
			reply(w, 200, upid(v.Node, "qmreboot", v.VMID))
		}
	})
	mux.HandleFunc("POST /nodes/{node}/qemu/{vmid}/status/reset", func(w http.ResponseWriter, r *http.Request) {
		if v := vm(w, r); v != nil {
			if !v.Running {
				reply(w, 500, "VM is not running")
				return
			}
			reply(w, 200, upid(v.Node, "qmreset", v.VMID))
		}
	})
	mux.HandleFunc("POST /nodes/{node}/qemu/{vmid}/status/stop", func(w http.ResponseWriter, r *http.Request) {
		if v := vm(w, r); v != nil {
			v.Running = false
			reply(w, 200, upid(v.Node, "qmstop", v.VMID))
		}
	})
	// Proxmox refuses to delete a running VM; it reads a DELETE's parameters from the query.
	mux.HandleFunc("DELETE /nodes/{node}/qemu/{vmid}", func(w http.ResponseWriter, r *http.Request) {
		if v := vm(w, r); v != nil {
			if v.Running {
				reply(w, 500, "VM is running")
				return
			}
			if r.URL.Query().Get("destroy-unreferenced-disks") != "1" || r.ContentLength > 0 {
				reply(w, 400, "destroy-unreferenced-disks=1 is expected in the query")
				return
			}
			for _, other := range s.vms {
				if other.Base == v.VMID {
					reply(w, 500, fmt.Sprintf("base volume of template %d is used by linked clone %d", v.VMID, other.VMID))
					return
				}
			}
			delete(s.vms, v.VMID)
			reply(w, 200, upid(v.Node, "qmdestroy", v.VMID))
		}
	})
	mux.HandleFunc("GET /nodes/{node}/tasks/{upid}/status", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]string{"status": "stopped", "exitstatus": "OK"})
	})
	mux.ServeHTTP(w, r)
}
