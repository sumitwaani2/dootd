// Command dootd is a minimal single-binary PaaS for small Zig/C + SQLite web apps.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/sumitwaani2/dootd/internal/buildinfo"
)

const usage = `dootd - minimal PaaS for small Zig/C + SQLite web apps

Usage:
  dootd <command>

Commands:
  serve            Run the dootd service (normally started by systemd)
  init             Interactive first-time setup
  reset-password   Reset the dashboard admin password
  version          Print version information
  help             Show this help
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, buildinfo.String())
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	case "serve", "init", "reset-password":
		fmt.Fprintf(stderr, "dootd: %q is not implemented yet in %s\n", args[0], buildinfo.Version)
		return 1
	default:
		fmt.Fprintf(stderr, "dootd: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
