// Command mamela-proxmox makes the scripts a person reviews and runs as root on a Proxmox node for
// Mamela's Proxmox provider: the bootstrap (pool, roles, users, tokens, ACLs) and the cleanup of
// unused uploads. It reads mamela.providers.toml and makes no request to Proxmox.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ieyoukan/mamela-proxmox/admin"
)

const usage = `usage:
  mamela-proxmox bootstrap   [--providers mamela.providers.toml] [--name proxmox]   print the root setup script
  mamela-proxmox permissions [--providers mamela.providers.toml] [--name proxmox]   explain every privilege of the operating token
  mamela-proxmox cleanup     [--providers mamela.providers.toml] [--name proxmox]   print a root script removing unused uploads
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	providers := fs.String("providers", "mamela.providers.toml", "the providers' API settings")
	name := fs.String("name", "proxmox", "the provider's name in it")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	c, err := admin.Load(*providers, *name)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	switch args[0] {
	case "bootstrap":
		fmt.Fprint(stdout, admin.Bootstrap(c))
	case "permissions":
		fmt.Fprint(stdout, admin.Permissions(c))
	case "cleanup":
		fmt.Fprint(stdout, admin.Cleanup(c))
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	return 0
}
