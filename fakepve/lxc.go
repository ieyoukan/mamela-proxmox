package fakepve

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// containers adds the container (lxc) calls: their configuration (with its digest, the description
// as Proxmox gives it back: decoded, ending in a newline), the root filesystem's growth, their
// status and power. A container is refused what a real one refuses: a smaller root filesystem, a
// mount point, starting one that runs, stopping one that does not.
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
	power("shutdown", true, func(v *VM) { v.Running = false })
	power("stop", true, func(v *VM) { v.Running = false })
	power("reboot", true, func(v *VM) { v.Uptime = 5 })
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
