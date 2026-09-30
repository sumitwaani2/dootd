package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/sumitwaani2/dootd/contrib/systemd"
	"github.com/sumitwaani2/dootd/internal/auth"
	"github.com/sumitwaani2/dootd/internal/buildinfo"
	"github.com/sumitwaani2/dootd/internal/edge"
	"github.com/sumitwaani2/dootd/internal/layout"
	"github.com/sumitwaani2/dootd/internal/secrets"
	"github.com/sumitwaani2/dootd/internal/store"
)

// runSetupHost is the second half of install.sh (docs/architecture.md):
// directories, master key, systemd unit, a new one-time password, start
// dootd, print how to sign in. install.sh stops dootd before it replaces
// the binary; running this again is safe.
func runSetupHost(stdout, stderr io.Writer) int {
	if err := setupHost(stdout); err != nil {
		fmt.Fprintln(stderr, "dootd setup-host:", err)
		return 1
	}
	return 0
}

func setupHost(out io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("must run as root")
	}
	if serviceActive() {
		return errors.New("dootd is running; stop it first (systemctl stop dootd)")
	}
	lay := layout.Default()
	if err := layout.EnsureBase(lay); err != nil {
		return err
	}
	if _, err := secrets.LoadOrCreate(layout.MasterKey); err != nil {
		return err
	}
	if cur, err := os.ReadFile(systemd.UnitPath); err != nil || string(cur) != systemd.Unit {
		tmp := systemd.UnitPath + ".tmp"
		if err := os.WriteFile(tmp, []byte(systemd.Unit), 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, systemd.UnitPath); err != nil {
			return err
		}
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", "--quiet", "dootd"); err != nil {
		return err
	}

	// The one-time password is written while dootd is stopped (it caches
	// the state in memory).
	ctx := context.Background()
	st, err := store.Open(ctx, lay.DBPath())
	if err != nil {
		return err
	}
	pw, until, err := auth.NewSetupPassword(ctx, st)
	var domain string
	if v, ok, _ := st.GetSetting(ctx, edge.SettingDashboardDomain); ok {
		domain = string(v)
	}
	st.Close()
	if err != nil {
		return err
	}
	if err := os.Chmod(lay.DBPath(), 0o600); err != nil {
		return err
	}

	if err := systemctl("start", "dootd"); err != nil {
		return err
	}
	ready := waitListening("127.0.0.1:443", 30*time.Second)

	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	ip, _ := edge.DetectIP(dctx, "tcp4")
	cancel()
	if ip == "" {
		ip = "<this server's IP address>"
	}

	fmt.Fprintf(out, "\ndootd %s is running.\n\n", buildinfo.Version)
	if !ready {
		fmt.Fprintln(out, "  Warning: dootd is not answering on port 443 yet; see: journalctl -u dootd")
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "  Open:              https://%s\n", ip)
	fmt.Fprintf(out, "                     Your browser warns that the certificate is not trusted: expected here, continue.\n")
	fmt.Fprintf(out, "  One-time password: %s\n", pw)
	fmt.Fprintf(out, "                     Leave the email empty. Works once, until %s.\n", until.UTC().Format("2006-01-02 15:04 UTC"))
	if domain != "" {
		fmt.Fprintf(out, "\n  Dashboard:         https://%s\n", domain)
		fmt.Fprintf(out, "                     Sign in there as usual; the address above is only needed if that fails.\n")
	}
	fmt.Fprintln(out)
	return nil
}

func waitListening(addr string, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func serviceActive() bool {
	return exec.Command("systemctl", "is-active", "--quiet", "dootd").Run() == nil
}
