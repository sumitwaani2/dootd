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
  init             First-time setup: admin, dashboard domain, Cloudflare (run once after install.sh)
  init --restore   Rebuild a server from a recovery kit and the backup bucket
  reset-password   Reset the dashboard admin password
  ctl              Control a running dootd (deploy, rollback, status, logs)
  update           Check for / install a new dootd release (same as Settings → Updates)
  setup-host       Create directories, the master key and the systemd unit (used by install.sh)
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
	case "serve":
		return runServe(args[1:], stderr)
	case "ctl":
		return runCtl(args[1:], stdout, stderr)
	case "reset-password":
		// Shortcut for: dootd ctl admin set-password [--email E]
		return runCtl(append([]string{"admin", "set-password"}, args[1:]...), stdout, stderr)
	case "init":
		return runInit(args[1:], stdout, stderr)
	case "update":
		// Shortcut for: dootd ctl update [--install]
		return runCtl(append([]string{"update"}, args[1:]...), stdout, stderr)
	case "update-guard":
		return runUpdateGuard(args[1:], stdout, stderr)
	case "setup-host":
		return runSetupHost(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "dootd: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
