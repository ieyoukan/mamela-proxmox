// Command proxmox-catalogs writes the provider's API catalogs (mamela-catalog/v1) into a
// directory: build.sh writes them into the Pome (pome/catalogs). The catalog is generated from the
// provider's manifest and the reviewed table (catalogs/review.go).
//
// With -schema (Proxmox VE's apidoc.js), it first checks the catalog and the permission catalog
// against the schema, which must be the one pve-api.lock names; with -update-lock, it checks them
// against a new one and, if they hold, names it in the lock (-pve-manager, its release).
//
//	go run ./cmd/proxmox-catalogs pome/catalogs
//	go run ./cmd/proxmox-catalogs -schema apidoc.js pome/catalogs
//	go run ./cmd/proxmox-catalogs -schema apidoc.js -pve-manager 9.3.1 -update-lock pome/catalogs
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ieyoukan/mamela-proxmox/catalogs"
	"github.com/ieyoukan/mamela-proxmox/proxmox"
)

func main() {
	schemaFile := flag.String("schema", os.Getenv("PVE_APIDOC"), "Proxmox VE's API schema (apidoc.js of pve-docs) to check against (default: $PVE_APIDOC)")
	lockFile := flag.String("lock", "catalogs/pve-api.lock", "the lock that names the schema")
	update := flag.Bool("update-lock", false, "check against a new schema and name it in the lock")
	release := flag.String("pve-manager", "", "with -update-lock: the pve-manager release the schema is from")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: proxmox-catalogs [-schema apidoc.js [-pve-manager <release> -update-lock]] <dir>")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 || (*update && (*schemaFile == "" || *release == "")) {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(flag.Arg(0), *schemaFile, *lockFile, *update, *release); err != nil {
		fmt.Fprintln(os.Stderr, "proxmox-catalogs:", err)
		os.Exit(1)
	}
}

func run(dir, schemaFile, lockFile string, update bool, release string) error {
	all, err := catalogs.All()
	if err != nil {
		return err
	}
	if schemaFile != "" {
		data, err := os.ReadFile(schemaFile)
		if err != nil {
			return err
		}
		s, err := catalogs.ReadSchema(data)
		if err != nil {
			return err
		}
		lock, err := catalogs.Locked()
		if err != nil {
			return err
		}
		if !update && s.SHA256 != lock.SHA256 {
			return fmt.Errorf("%s (sha256 %s) is not the schema %s names (pve-manager %s, sha256 %s): give that one, "+
				"or check against this one with -pve-manager <its release> -update-lock", schemaFile, s.SHA256, lockFile, lock.PVEManager, lock.SHA256)
		}
		errs := catalogs.Validate(s, proxmox.New().Manifest(), all["proxmox"], catalogs.Reviewed)
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, "  "+e.Error())
		}
		if len(errs) > 0 {
			return fmt.Errorf("%d problems with Proxmox VE's API schema %s: nothing was written", len(errs), schemaFile)
		}
		if update {
			if err := os.WriteFile(lockFile, catalogs.Lock{PVEManager: release, SHA256: s.SHA256}.Encode(), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "the catalog holds against pve-manager %s's API schema (sha256 %s): %s names it\n", release, s.SHA256, lockFile)
		} else {
			fmt.Fprintf(os.Stderr, "the catalog holds against pve-manager %s's API schema\n", lock.PVEManager)
		}
	}
	for name, c := range all {
		data, err := catalogs.Encode(c)
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, name+".json"), data, 0o644)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
