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
	Config      map[string]string // other parameters, as created
	Serial      int               // bumped on every change; the digest is derived from it
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
}

// New returns a fake with no VMs and empty storages.
func New() *Server { return &Server{vms: map[int64]*VM{}, volumes: map[string]string{}} }

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
			status, uptime := "stopped", 0
			if v.Running {
				status, uptime = "running", 120
			}
			out = append(out, map[string]any{"vmid": v.VMID, "name": v.Name, "node": v.Node, "pool": v.Pool, "type": "qemu",
				"status": status, "uptime": uptime})
		}
		reply(w, 200, out)
	})
	vm := func(w http.ResponseWriter, r *http.Request) *VM {
		id, _ := strconv.ParseInt(r.PathValue("vmid"), 10, 64)
		v, ok := s.vms[id]
		if !ok || v.Node != r.PathValue("node") {
			reply(w, 500, nil)
			return nil
		}
		return v
	}
	mux.HandleFunc("GET /nodes/{node}/qemu/{vmid}/config", func(w http.ResponseWriter, r *http.Request) {
		if v := vm(w, r); v != nil {
			cfg := map[string]any{"name": v.Name, "cores": v.Cores, "memory": strconv.FormatInt(v.Memory, 10),
				"description": v.Description, "digest": v.digest(), "scsi0": fmt.Sprintf("local-lvm:vm-%d-disk-0,discard=on,size=%dG", v.VMID, v.DiskGiB)}
			if v.Tags != "" {
				cfg["tags"] = v.Tags
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
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			if p.FormName() == "filename" {
				name, content = p.FileName(), string(b)
			} else {
				form[p.FormName()] = string(b)
			}
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
		if _, ok := s.volumes[seed]; !ok {
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
			Description: r.PostForm.Get("description"), Cores: cores, Memory: mem, DiskGiB: 4, Config: cfg}
		reply(w, 200, upid(r.PathValue("node"), "qmcreate", id))
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
		v.Serial++
		reply(w, 200, nil)
	})
	mux.HandleFunc("POST /nodes/{node}/qemu/{vmid}/status/start", func(w http.ResponseWriter, r *http.Request) {
		if v := vm(w, r); v != nil {
			v.Running = true
			reply(w, 200, upid(v.Node, "qmstart", v.VMID))
		}
	})
	mux.HandleFunc("POST /nodes/{node}/qemu/{vmid}/status/reboot", func(w http.ResponseWriter, r *http.Request) {
		if v := vm(w, r); v != nil {
			reply(w, 200, upid(v.Node, "qmreboot", v.VMID))
		}
	})
	mux.HandleFunc("GET /nodes/{node}/tasks/{upid}/status", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]string{"status": "stopped", "exitstatus": "OK"})
	})
	mux.ServeHTTP(w, r)
}
