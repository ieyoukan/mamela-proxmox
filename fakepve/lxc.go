package fakepve

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// containers adds the container (lxc) calls: their configuration (with its digest, the description
// as Proxmox gives it back: decoded, ending in a newline), the root filesystem's growth, their
// status and power, and deleting them. A container is refused what a real one refuses: a smaller
// root filesystem, a mount point, starting one that runs, stopping one that does not, deleting one
// that runs or is protected or locked. A shutdown that is not forced, of a container that does not
// stop (Stubborn), ends its task with an error, and the container keeps running.
func (s *Server) containers(mux *http.ServeMux, changed changedFunc) {
	ct := func(w http.ResponseWriter, r *http.Request) *VM {
		id, _ := strconv.ParseInt(r.PathValue("vmid"), 10, 64)
		v, ok := s.vms[id]
		if !ok || v.Node != r.PathValue("node") || v.Type != "lxc" {
			reply(w, 500, fmt.Sprintf("Configuration file 'nodes/%s/lxc/%d.conf' does not exist", r.PathValue("node"), id))
			return nil
		}
		return v
	}
	mux.HandleFunc("GET /nodes/{node}/lxc/{vmid}/config", func(w http.ResponseWriter, r *http.Request) {
		v := ct(w, r)
		if v == nil {
			return
		}
		cfg := map[string]any{"arch": "amd64", "hostname": v.Name, "digest": v.digest(), "memory": v.Memory, "swap": v.Swap,
			"rootfs": rootfsOption(v), "onboot": boolNumber(v.Onboot), "ostype": "ubuntu", "unprivileged": 1,
			"net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:10:00,ip=dhcp,type=veth"}
		if v.Cores != 0 {
			cfg["cores"] = v.Cores
		}
		if v.Tags != "" {
			cfg["tags"] = v.Tags
		}
		if v.Description != "" {
			cfg["description"] = v.Description
		}
		if v.Template {
			cfg["template"] = 1
		}
		if v.Protection {
			cfg["protection"] = 1
		}
		if v.Lock != "" {
			cfg["lock"] = v.Lock
		}
		reply(w, 200, cfg)
	})
	mux.HandleFunc("PUT /nodes/{node}/lxc/{vmid}/config", func(w http.ResponseWriter, r *http.Request) {
		v := ct(w, r)
		if v == nil || !changed(w, r, v) {
			return
		}
		next := *v
		for k, vals := range r.PostForm {
			val := vals[0]
			var err error
			switch k {
			case "digest":
			case "cores":
				next.Cores, err = strconv.ParseInt(val, 10, 64)
			case "memory":
				next.Memory, err = strconv.ParseInt(val, 10, 64)
			case "swap":
				next.Swap, err = strconv.ParseInt(val, 10, 64)
			case "onboot":
				next.Onboot = val == "1"
			case "tags":
				next.Tags = val
			case "description":
				// Kept as comment lines: read back, it ends in a newline.
				if val != "" && !strings.HasSuffix(val, "\n") {
					val += "\n"
				}
				if len(val) > 8192 {
					reply(w, 400, "description: value may only be 8192 characters long")
					return
				}
				next.Description = val
			default:
				reply(w, 400, "the fake does not take parameter "+k)
				return
			}
			if err != nil {
				reply(w, 400, k+": "+err.Error())
				return
			}
		}
		next.Serial++
		*v = next
		reply(w, 200, nil)
	})
	mux.HandleFunc("PUT /nodes/{node}/lxc/{vmid}/resize", func(w http.ResponseWriter, r *http.Request) {
		v := ct(w, r)
		if v == nil || !changed(w, r, v) {
			return
		}
		size := r.PostForm.Get("size")
		n, err := strconv.ParseInt(strings.TrimSuffix(size, "G"), 10, 64)
		switch {
		case r.PostForm.Get("disk") != "rootfs":
			reply(w, 400, "the fake resizes only rootfs")
			return
		case err != nil || !strings.HasSuffix(size, "G"):
			reply(w, 400, "size: value does not match the regex pattern")
			return
		case n < v.DiskGiB:
			reply(w, 500, "unable to shrink disk size")
			return
		}
		v.DiskGiB = n
		v.Serial++
		reply(w, 200, upid(v.Node, "resize", v.VMID))
	})
	mux.HandleFunc("GET /nodes/{node}/lxc/{vmid}/status/current", func(w http.ResponseWriter, r *http.Request) {
		v := ct(w, r)
		if v == nil {
			return
		}
		status, uptime := "stopped", int64(0)
		if v.Running {
			status, uptime = "running", 120
			if v.Uptime != 0 {
				uptime = v.Uptime
			}
		}
		reply(w, 200, map[string]any{"vmid": v.VMID, "name": v.Name, "type": "lxc", "status": status, "uptime": uptime, "tags": v.Tags,
			"maxmem": v.Memory << 20, "maxswap": v.Swap << 20, "maxdisk": v.DiskGiB << 30, "cpus": max(v.Cores, 1)})
	})
	power := func(action string, wantRunning bool, then func(v *VM)) {
		mux.HandleFunc("POST /nodes/{node}/lxc/{vmid}/status/"+action, func(w http.ResponseWriter, r *http.Request) {
			v := ct(w, r)
			if v == nil {
				return
			}
			switch {
			case v.Template:
				reply(w, 500, "you can't start a CT if it's a template")
				return
			case v.Running != wantRunning && wantRunning:
				reply(w, 500, fmt.Sprintf("CT %d not running", v.VMID))
				return
			case v.Running != wantRunning:
				reply(w, 500, fmt.Sprintf("CT %d already running", v.VMID))
				return
			}
			then(v)
			reply(w, 200, upid(v.Node, "vz"+action, v.VMID))
		})
	}
	power("start", false, func(v *VM) { v.Running, v.Uptime = true, 0 })
	power("stop", true, func(v *VM) { v.Running = false })
	power("reboot", true, func(v *VM) { v.Uptime = 5 })
	// A shutdown (lxc-stop, waiting timeout seconds, 60 when not given): a stubborn container does
	// not stop, and unless forceStop is 1 its task fails and it keeps running.
	mux.HandleFunc("POST /nodes/{node}/lxc/{vmid}/status/shutdown", func(w http.ResponseWriter, r *http.Request) {
		v := ct(w, r)
		if v == nil {
			return
		}
		r.ParseForm()
		for k := range r.PostForm {
			if k != "timeout" && k != "forceStop" {
				reply(w, 400, "the fake does not take parameter "+k)
				return
			}
		}
		switch {
		case v.Template:
			reply(w, 500, "you can't start a CT if it's a template")
			return
		case !v.Running:
			reply(w, 500, fmt.Sprintf("CT %d not running", v.VMID))
			return
		}
		id := upid(v.Node, "vzshutdown", v.VMID)
		switch {
		case v.Stubborn && r.PostForm.Get("forceStop") != "1":
			s.failed[id] = "container did not stop"
		default:
			delete(s.failed, id) // the fake's UPIDs repeat
			v.Running = false
		}
		reply(w, 200, id)
	})
	// Deleting reads its parameters from the query (a DELETE), refuses a running, protected or
	// locked container (before any task, as Proxmox does), and expects purge and
	// destroy-unreferenced-disks (what the provider sends).
	mux.HandleFunc("DELETE /nodes/{node}/lxc/{vmid}", func(w http.ResponseWriter, r *http.Request) {
		v := ct(w, r)
		if v == nil {
			return
		}
		q := r.URL.Query()
		switch {
		case r.ContentLength > 0:
			reply(w, 400, "a DELETE's parameters are in the query")
		case q.Get("purge") != "1" || q.Get("destroy-unreferenced-disks") != "1" || q.Has("force") || len(q) != 2:
			reply(w, 400, "purge=1 and destroy-unreferenced-disks=1 (only) are expected in the query")
		case v.Protection:
			reply(w, 500, fmt.Sprintf("can't remove CT %d - protection mode enabled", v.VMID))
		case v.Lock != "":
			reply(w, 500, fmt.Sprintf("CT is locked (%s)", v.Lock))
		case v.Running:
			reply(w, 500, fmt.Sprintf("unable to destroy CT %d - container is running", v.VMID))
		default:
			delete(s.vms, v.VMID)
			reply(w, 200, upid(v.Node, "vzdestroy", v.VMID))
		}
	})
}

// rootfsOption writes a container's root filesystem as Proxmox does.
func rootfsOption(v *VM) string {
	st := v.DiskStorage
	if st == "" {
		st = "local-lvm"
	}
	return fmt.Sprintf("%s:vm-%d-disk-0,size=%dG", st, v.VMID, v.DiskGiB)
}

func boolNumber(b bool) int {
	if b {
		return 1
	}
	return 0
}
