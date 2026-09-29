package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/sumitwaani2/dootd/contrib/systemd"
	"github.com/sumitwaani2/dootd/internal/config"
	"github.com/sumitwaani2/dootd/internal/secrets"
	"github.com/sumitwaani2/dootd/internal/selfupdate"
)

// runSetupHost prepares the host after the binary is installed (called by
// install.sh): directories, master key and the systemd unit (enabled, not
// started; `dootd init` starts it). It is safe to run again.
func runSetupHost(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("setup-host", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", config.DefaultPath, "path to config.toml")
	noSystemd := fs.Bool("no-systemd", false, "do not install or enable the systemd unit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := setupHost(*cfgPath, !*noSystemd, stdout); err != nil {
		fmt.Fprintln(stderr, "dootd setup-host:", err)
		return 1
	}
	return 0
}

func setupHost(cfgPath string, withSystemd bool, out io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("must run as root")
	}
	cfg, err := config.Load(cfgPath, false)
	if err != nil {
		return err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}
	fmt.Fprintf(out, "directories: %s (0700), %s (0711)\n", dirOf(cfg.MasterKey), cfg.DataRoot)
	_, statErr := os.Stat(cfg.MasterKey)
	if _, err := secrets.LoadOrCreate(cfg.MasterKey); err != nil {
		return err
	}
	if statErr == nil {
		fmt.Fprintf(out, "master key: kept the existing %s\n", cfg.MasterKey)
	} else {
		fmt.Fprintf(out, "master key: created %s (0600). Download the recovery kit from the dashboard later.\n", cfg.MasterKey)
	}
	if !withSystemd {
		return nil
	}
	changed, err := installUnit()
	if err != nil {
		return err
	}
	if changed {
		fmt.Fprintf(out, "systemd unit: installed %s\n", systemd.UnitPath)
	} else {
		fmt.Fprintf(out, "systemd unit: %s is up to date\n", systemd.UnitPath)
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", "--quiet", "dootd"); err != nil {
		return err
	}
	fmt.Fprintln(out, "systemd unit: enabled (starts at boot; `dootd init` starts it now)")
	return nil
}

// installUnit writes the embedded unit if the installed one differs.
func installUnit() (bool, error) {
	if cur, err := os.ReadFile(systemd.UnitPath); err == nil && string(cur) == systemd.Unit {
		return false, nil
	}
	tmp := systemd.UnitPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(systemd.Unit), 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, systemd.UnitPath)
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

func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "/"
}

// prompter asks questions on the terminal. Without a terminal (automation)
// every answer must come from flags or environment variables.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
	tty bool
	fd  int
}

func newPrompter(out io.Writer) *prompter {
	fd := int(os.Stdin.Fd())
	p := &prompter{in: bufio.NewReader(os.Stdin), out: out, tty: term.IsTerminal(fd), fd: fd}
	if !p.tty {
		// `curl ... | sudo bash` style: stdin is the script, but a
		// terminal may still be attached.
		if f, err := os.Open("/dev/tty"); err == nil {
			if term.IsTerminal(int(f.Fd())) {
				p.in, p.tty, p.fd = bufio.NewReader(f), true, int(f.Fd())
			} else {
				f.Close()
			}
		}
	}
	return p
}

var errNoTTY = errors.New("no terminal to ask on")

// ask returns def when given, or the typed answer (def on empty input).
func (p *prompter) ask(question, def string) (string, error) {
	if !p.tty {
		if def != "" {
			return def, nil
		}
		return "", fmt.Errorf("%w: %s", errNoTTY, question)
	}
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", question, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", question)
	}
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	if line = strings.TrimSpace(line); line == "" {
		return def, nil
	}
	return line, nil
}

// secret reads a hidden value. It reads through the same buffered reader
// as ask (answers typed ahead are not lost) with echo switched off first.
func (p *prompter) secret(question string) (string, error) {
	if !p.tty {
		return "", fmt.Errorf("%w: %s", errNoTTY, question)
	}
	if t, err := unix.IoctlGetTermios(p.fd, unix.TCGETS); err == nil {
		quiet := *t
		quiet.Lflag &^= unix.ECHO
		quiet.Lflag |= unix.ICANON | unix.ISIG
		if unix.IoctlSetTermios(p.fd, unix.TCSETS, &quiet) == nil {
			defer unix.IoctlSetTermios(p.fd, unix.TCSETS, t)
		}
	}
	fmt.Fprintf(p.out, "%s: ", question)
	line, err := p.in.ReadString('\n')
	fmt.Fprintln(p.out)
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// yes asks a yes/no question; without a terminal it returns def.
func (p *prompter) yes(question string, def bool) bool {
	if !p.tty {
		return def
	}
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	fmt.Fprintf(p.out, "%s [%s]: ", question, hint)
	line, _ := p.in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	}
	return def
}

// runUpdateGuard is run by systemd before every start, from dootd.prev
// (see contrib/systemd/dootd.service and selfupdate.Guard).
func runUpdateGuard(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update-guard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", config.DefaultPath, "path to config.toml")
	binary := fs.String("binary", "/usr/local/bin/dootd", "the binary systemd starts")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath, false)
	if err != nil {
		fmt.Fprintln(stderr, "dootd update-guard:", err)
		return 1
	}
	msg, err := selfupdate.Guard(*binary, cfg.DataRoot)
	if err != nil {
		fmt.Fprintln(stderr, "dootd update-guard:", err)
		return 1
	}
	if msg != "" {
		fmt.Fprintln(stdout, "dootd update-guard:", msg)
	}
	return 0
}
