package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sumitwaani2/dootd/internal/auth"
	"github.com/sumitwaani2/dootd/internal/backup"
	"github.com/sumitwaani2/dootd/internal/cloudflare"
	"github.com/sumitwaani2/dootd/internal/config"
	"github.com/sumitwaani2/dootd/internal/control"
	"github.com/sumitwaani2/dootd/internal/edge"
	"github.com/sumitwaani2/dootd/internal/layout"
	"github.com/sumitwaani2/dootd/internal/s3"
	"github.com/sumitwaani2/dootd/internal/secrets"
	"github.com/sumitwaani2/dootd/internal/store"
	"github.com/sumitwaani2/dootd/internal/users"
)

const initUsage = `dootd init - first-time setup (run as root, once, right after install.sh)

Usage:
  dootd init [flags]                    set up a new server
  dootd init --restore KIT [flags]      rebuild a server from a recovery kit and the bucket

It asks for everything it needs. For automation, answer with flags and
environment variables instead (no terminal = no questions):
  DOOTD_ADMIN_PASSWORD     admin password (new server)
  DOOTD_CLOUDFLARE_TOKEN   Cloudflare API token (new server)
  DOOTD_S3_SECRET          S3 secret access key (--restore)

Flags:
`

type initOpts struct {
	cfgPath, socket         string
	email, domain           string
	ipv4, ipv6              string
	yes, noStart, sslStrict bool
	restore                 string
	s3AccessKey, s3Region   string
	noDeploy                bool
	cloudflareAPI           string
}

func runInit(args []string, stdout, stderr io.Writer) int {
	var o initOpts
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, initUsage); fs.PrintDefaults() }
	fs.StringVar(&o.cfgPath, "config", config.DefaultPath, "path to config.toml")
	fs.StringVar(&o.socket, "socket", control.DefaultSocket, "control socket")
	fs.StringVar(&o.email, "email", "", "admin email")
	fs.StringVar(&o.domain, "domain", "", "dashboard domain, e.g. dootd.example.com")
	fs.StringVar(&o.ipv4, "ipv4", "", "public IPv4 (default: detected)")
	fs.StringVar(&o.ipv6, "ipv6", "", `public IPv6, or "off" for no AAAA records (default: detected)`)
	fs.BoolVar(&o.yes, "yes", false, "accept detected values and confirm re-running setup")
	fs.BoolVar(&o.sslStrict, "ssl-strict", false, "set the dashboard zone's SSL/TLS mode to Full (strict) without asking")
	fs.BoolVar(&o.noStart, "no-start", false, "do not start the service at the end")
	fs.StringVar(&o.restore, "restore", "", "recovery kit file: restore dootd.db and every app's data from the bucket")
	fs.StringVar(&o.s3AccessKey, "s3-access-key", "", "S3 access key ID (--restore)")
	fs.StringVar(&o.s3Region, "s3-region", "", `S3 region (--restore; default: from the kit, else "auto")`)
	fs.BoolVar(&o.noDeploy, "no-deploy", false, "--restore: do not queue deploys of the restored apps")
	fs.StringVar(&o.cloudflareAPI, "cloudflare-api", "", "Cloudflare API base URL (tests only)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	var err error
	if o.restore != "" {
		err = initRestore(o, stdout)
	} else {
		err = initNew(o, stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, "dootd init:", err)
		return 1
	}
	return 0
}

func step(out io.Writer, format string, a ...any) {
	fmt.Fprintf(out, "\n\033[1;34m==>\033[0m "+format+"\n", a...)
}

// prepare loads the config and stops a running service (init rewrites
// state it holds open).
func prepare(o initOpts, out io.Writer) (*config.Config, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("must run as root (sudo dootd init)")
	}
	cfg, err := config.Load(o.cfgPath, false)
	if err != nil {
		return nil, err
	}
	if o.cloudflareAPI != "" {
		cfg.Edge.CloudflareAPI = o.cloudflareAPI
	}
	if cfg.Edge.Listen == "off" {
		return nil, errors.New(`edge.listen is "off" in the config; dootd init sets up the public dashboard, remove that line first`)
	}
	if err := cfg.EnsureDirs(); err != nil {
		return nil, err
	}
	if serviceActive() {
		fmt.Fprintln(out, "Stopping dootd for the setup (it is started again at the end).")
		if err := systemctl("stop", "dootd"); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// ---------------------------------------------------------------- new server

func initNew(o initOpts, out io.Writer) error {
	p := newPrompter(out)
	if os.Geteuid() == 0 {
		if cur, err := config.Load(o.cfgPath, false); err == nil && cur.Edge.DashboardDomain != "" {
			if !o.yes && !p.yes(fmt.Sprintf("dootd is already set up for %s. Run the setup again? This replaces the admin password", cur.Edge.DashboardDomain), false) {
				return errors.New("cancelled; nothing was changed (use `dootd reset-password` to only change the password)")
			}
		}
	}
	cfg, err := prepare(o, out)
	if err != nil {
		return err
	}
	box, err := secrets.LoadOrCreate(cfg.MasterKey)
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	os.Chmod(cfg.DBPath(), 0o600)
	au := auth.New(st)

	step(out, "Dashboard admin")
	email, err := p.ask("Admin email", o.email)
	if err != nil {
		return fmt.Errorf("%w (pass --email)", err)
	}
	if email, err = auth.NormalizeEmail(email); err != nil {
		return err
	}
	pw := os.Getenv("DOOTD_ADMIN_PASSWORD")
	if pw == "" {
		if pw, err = p.secret(fmt.Sprintf("Password (at least %d characters)", auth.MinPasswordLen)); err != nil {
			return fmt.Errorf("%w (set DOOTD_ADMIN_PASSWORD)", err)
		}
		again, err := p.secret("Repeat password")
		if err != nil {
			return err
		}
		if again != pw {
			return errors.New("passwords do not match")
		}
	}
	if len([]rune(pw)) < auth.MinPasswordLen {
		return fmt.Errorf("password must be at least %d characters", auth.MinPasswordLen)
	}

	step(out, "Dashboard domain")
	def := o.domain
	if def == "" {
		def = cfg.Edge.DashboardDomain
	}
	domain, err := p.ask("Dashboard domain (e.g. dootd.example.com)", def)
	if err != nil {
		return fmt.Errorf("%w (pass --domain)", err)
	}
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if err := edge.ValidHostname(domain); err != nil {
		return err
	}

	step(out, "Cloudflare API token")
	fmt.Fprintln(out, "Needs: Zone Read, DNS Edit, SSL and Certificates Edit, Zone Settings Edit (docs/cloudflare-setup.md).")
	token := strings.TrimSpace(os.Getenv("DOOTD_CLOUDFLARE_TOKEN"))
	if token == "" {
		if token, err = p.secret("API token"); err != nil {
			return fmt.Errorf("%w (set DOOTD_CLOUDFLARE_TOKEN)", err)
		}
	}
	cf := &cloudflare.Client{Token: token, Base: cfg.Edge.CloudflareAPI}
	zone, mode, err := checkToken(ctx, cf, domain, out)
	if err != nil {
		return err
	}
	if mode != "strict" {
		fmt.Fprintf(out, "Zone %s uses SSL/TLS mode %q. dootd needs Full (strict), which affects every site in the zone.\n", zone.Name, mode)
		if o.sslStrict || p.yes("Set "+zone.Name+" to Full (strict) now?", false) {
			if err := cf.SetSSLMode(ctx, zone.ID, "strict"); err != nil {
				return err
			}
			fmt.Fprintln(out, "SSL/TLS mode set to Full (strict).")
		} else {
			fmt.Fprintln(out, "Left unchanged; the dashboard shows a one-click fix.")
		}
	}

	step(out, "Public IP addresses")
	v4, v6, err := chooseIPs(ctx, cfg, o, p, out)
	if err != nil {
		return err
	}

	step(out, "Saving")
	cfg.Edge.DashboardDomain = domain
	if err := cfg.Save(o.cfgPath); err != nil {
		return err
	}
	fmt.Fprintf(out, "config: %s\n", o.cfgPath)
	if err := au.SetAdmin(ctx, email, pw); err != nil {
		return err
	}
	fmt.Fprintf(out, "admin: %s (all sessions signed out)\n", email)

	step(out, "DNS record, Origin CA certificate and Authenticated Origin Pulls for %s", domain)
	if err := edgeSetup(ctx, cfg, st, box, v4, v6, token, nil, out); err != nil {
		return err
	}
	st.Close()
	if err := startService(o, out); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nDone. Open https://%s and sign in as %s.\n", domain, email)
	fmt.Fprintln(out, "Next: Settings → GitHub token and S3 bucket, then Apps → Add app. Download the recovery kit once the bucket is set.")
	return nil
}

// checkToken verifies the token, finds the zone of domain and checks the
// read side of each permission dootd needs. It returns the SSL mode.
func checkToken(ctx context.Context, cf *cloudflare.Client, domain string, out io.Writer) (cloudflare.Zone, string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := cf.Verify(ctx); err != nil {
		return cloudflare.Zone{}, "", fmt.Errorf("token rejected: %w", err)
	}
	zone, err := cf.ZoneFor(ctx, domain)
	if err != nil {
		return zone, "", err
	}
	fmt.Fprintf(out, "token active; %s is in zone %s\n", domain, zone.Name)
	var missing []string
	perm := func(name string, err error) {
		if err != nil {
			missing = append(missing, fmt.Sprintf("  %s: %v", name, err))
		}
	}
	_, err = cf.DNSRecords(ctx, zone.ID, domain)
	perm("DNS Edit", err)
	mode, err := cf.SSLMode(ctx, zone.ID)
	perm("Zone Settings Edit", err)
	_, err = cf.ListAOPCerts(ctx, zone.ID)
	perm("SSL and Certificates Edit", err)
	if len(missing) > 0 {
		return zone, "", fmt.Errorf("the token cannot do everything dootd needs:\n%s", strings.Join(missing, "\n"))
	}
	fmt.Fprintln(out, "permissions look right (DNS, SSL and Certificates, Zone Settings)")
	return zone, mode, nil
}

// chooseIPs detects the public addresses and lets the operator confirm
// them. Values that differ from what detection finds are saved to the
// config, so dootd serve uses them too.
func chooseIPs(ctx context.Context, cfg *config.Config, o initOpts, p *prompter, out io.Writer) (string, string, error) {
	detect := func(network string) string {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ip, err := edge.DetectIP(dctx, network)
		if err != nil {
			return ""
		}
		return ip
	}
	v4 := o.ipv4
	if v4 == "" {
		v4 = cfg.Edge.PublicIPv4
	}
	det4 := ""
	if v4 == "" {
		if det4 = detect("tcp4"); det4 != "" {
			fmt.Fprintf(out, "detected IPv4 %s\n", det4)
		}
		if !o.yes {
			a, err := p.ask("Public IPv4 for the DNS records", det4)
			if err != nil {
				return "", "", fmt.Errorf("could not detect the public IPv4 (pass --ipv4): %w", err)
			}
			v4 = a
		} else {
			v4 = det4
		}
	}
	if a, err := netip.ParseAddr(v4); err != nil || !a.Is4() {
		return "", "", fmt.Errorf("public IPv4 %q is not an IPv4 address (pass --ipv4)", v4)
	}
	if v4 != det4 {
		cfg.Edge.PublicIPv4 = v4
	}

	v6 := o.ipv6
	if v6 == "" {
		v6 = cfg.Edge.PublicIPv6
	}
	det6 := ""
	if v6 == "" {
		if det6 = detect("tcp6"); det6 != "" {
			fmt.Fprintf(out, "detected IPv6 %s\n", det6)
		} else {
			det6 = "off"
			fmt.Fprintln(out, "no public IPv6 detected")
		}
		v6 = det6
		if !o.yes && p.tty {
			a, _ := p.ask(`Public IPv6 for AAAA records ("off" for none)`, det6)
			v6 = a
		}
	}
	if v6 != "off" {
		if a, err := netip.ParseAddr(v6); err != nil || !a.Is6() {
			return "", "", fmt.Errorf(`public IPv6 %q is not an IPv6 address (or "off")`, v6)
		}
	}
	if v6 != det6 {
		cfg.Edge.PublicIPv6 = v6
	}
	fmt.Fprintf(out, "using IPv4 %s, IPv6 %s\n", v4, v6)
	return v4, v6, nil
}

// edgeSetup stores the token (when given) and runs one Cloudflare sync for
// the dashboard and routes: DNS records, Origin CA certificates and AOP.
// Errors for the dashboard host are fatal, errors for apps only reported.
func edgeSetup(ctx context.Context, cfg *config.Config, st *store.Store, box *secrets.Box, v4, v6, token string, routes []edge.Route, out io.Writer) error {
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := edge.NewManager(ctx, edge.Config{
		Listen: cfg.Edge.Listen, DashboardHost: cfg.Edge.DashboardDomain, PublicIPv4: v4, PublicIPv6: v6,
		AOP: cfg.Edge.AOPEnabled(), APIBase: cfg.Edge.CloudflareAPI, DataRoot: cfg.DataRoot,
	}, st, box, lg)
	if err != nil {
		return err
	}
	m.Router = &edge.Router{DashboardHost: cfg.Edge.DashboardDomain, Log: lg}
	if token != "" {
		if _, err := m.SetToken(ctx, token); err != nil {
			return err
		}
		fmt.Fprintln(out, "Cloudflare token saved (encrypted)")
	}
	m.SetRoutes(routes)
	fmt.Fprintln(out, "syncing with Cloudflare (waits until Cloudflare has activated the AOP certificate, up to 3 minutes)...")
	sctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	s, err := m.Sync(sctx)
	printEdge(out, s)
	for _, h := range s.Hosts {
		if h.App == "" && h.Error != "" {
			return fmt.Errorf("dashboard %s: %s (fix it and run dootd init again)", h.Host, h.Error)
		}
	}
	for _, z := range s.Zones {
		if strings.HasSuffix(cfg.Edge.DashboardDomain, z.Name) && cfg.Edge.AOPEnabled() && z.AOP != "enforced" {
			return fmt.Errorf("authenticated origin pulls are not active for zone %s yet: %s (run dootd init again in a few minutes)", z.Name, z.Error)
		}
	}
	if err != nil {
		fmt.Fprintf(out, "warning: some hosts need attention (the dashboard shows them): %v\n", err)
	}
	return nil
}

func startService(o initOpts, out io.Writer) error {
	if o.noStart {
		fmt.Fprintln(out, "\nNot starting dootd (--no-start). Start it with: systemctl start dootd")
		return nil
	}
	step(out, "Starting dootd")
	if err := systemctl("enable", "--quiet", "dootd"); err != nil {
		return err
	}
	if err := systemctl("restart", "dootd"); err != nil {
		return err
	}
	c := newCtlClient(o.socket)
	for i := 0; i < 60; i++ {
		if err := c.do(http.MethodGet, "/v1/apps", nil, nil); err == nil {
			fmt.Fprintln(out, "dootd is running")
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("dootd did not come up within 30s; see journalctl -u dootd")
}

// ---------------------------------------------------------------- restore

func initRestore(o initOpts, out io.Writer) error {
	raw, err := os.ReadFile(o.restore)
	if err != nil {
		return err
	}
	kit, err := backup.ParseKit(string(raw))
	if err != nil {
		return err
	}
	if kit.Bucket == "" {
		return errors.New("this recovery kit has no bucket: backups were only kept on the old server, so there is nothing to restore from")
	}
	p := newPrompter(out)
	cfg, err := config.Load(o.cfgPath, false)
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfg.DBPath()); err == nil {
		return fmt.Errorf("%s already exists: --restore rebuilds a fresh server (move %s away first if you really want to replace it)", cfg.DBPath(), cfg.DataRoot)
	}
	fmt.Fprintf(out, "Recovery kit of host %s (%s, dashboard %s, created %s).\n", kit.HostID, kit.Server, kit.Dashboard, kit.Created.Format(time.DateTime))

	step(out, "Bucket %s at %s", kit.Bucket, kit.Endpoint)
	ak, err := p.ask("S3 access key ID", o.s3AccessKey)
	if err != nil {
		return fmt.Errorf("%w (pass --s3-access-key)", err)
	}
	sk := os.Getenv("DOOTD_S3_SECRET")
	if sk == "" {
		if sk, err = p.secret("S3 secret access key"); err != nil {
			return fmt.Errorf("%w (set DOOTD_S3_SECRET)", err)
		}
	}
	region := o.s3Region
	if region == "" {
		region = kit.Region
	}
	sc := s3.Config{Endpoint: kit.Endpoint, Region: region, Bucket: kit.Bucket, Prefix: kit.Prefix, AccessKey: ak, SecretKey: sk}
	if err := sc.Normalize(); err != nil {
		return err
	}
	cl, err := s3.New(sc)
	if err != nil {
		return err
	}
	ctx := context.Background()
	lctx, cancel := context.WithTimeout(ctx, time.Minute)
	selfObjs, err := cl.List(lctx, backup.KeyPrefix(sc.Prefix, kit.HostID, backup.SelfApp))
	cancel()
	if err != nil {
		return err
	}
	newest, ok := backup.Newest(selfObjs)
	if !ok {
		return fmt.Errorf("no dootd.db backup under %s in the bucket", backup.KeyPrefix(sc.Prefix, kit.HostID, backup.SelfApp))
	}

	cfg, err = prepare(o, out)
	if err != nil {
		return err
	}
	box, err := secrets.Install(cfg.MasterKey, kit.MasterKey)
	if errors.Is(err, secrets.ErrKeyMismatch) {
		// install.sh created a fresh key; with no dootd.db nothing was
		// encrypted with it yet. Keep it aside rather than deleting it.
		aside := cfg.MasterKey + ".replaced-" + time.Now().UTC().Format("20060102T150405Z")
		if err := os.Rename(cfg.MasterKey, aside); err != nil {
			return err
		}
		fmt.Fprintf(out, "the unused master key created by install.sh was moved to %s\n", aside)
		box, err = secrets.Install(cfg.MasterKey, kit.MasterKey)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "master key installed at %s\n", cfg.MasterKey)
	staging := filepath.Join(cfg.DataRoot, "backups", "staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return err
	}
	work, err := os.MkdirTemp(staging, "rebuild-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	step(out, "dootd.db from %s", newest.Key)
	if err := restoreSelfDB(ctx, cl, newest.Key, work, cfg.DBPath()); err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	os.Chmod(cfg.DBPath(), 0o600)
	lay := layout.Layout{Root: cfg.DataRoot}
	bs := &backup.Service{Layout: lay, Store: st, Box: box}
	if _, _, err := bs.S3Config(ctx); err != nil {
		return fmt.Errorf("the kit's master key does not open this dootd.db (%w); is it the kit of host %s?", err, kit.HostID)
	}
	if err := resetForNewServer(ctx, st); err != nil {
		return err
	}
	admin, _ := auth.New(st).Admin(ctx)
	fmt.Fprintf(out, "restored settings, apps and history (admin %s); schema up to date\n", orDash(admin))

	type restoredApp struct {
		name, domain string
		port         int
		running      bool
	}
	var list []restoredApp
	rows, err := st.Reader().QueryContext(ctx, `SELECT a.name, a.domain, a.port, COALESCE(s.desired_state, '') FROM apps a
		LEFT JOIN app_state s ON s.app = a.name ORDER BY a.name`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a restoredApp
		var state string
		if err := rows.Scan(&a.name, &a.domain, &a.port, &state); err != nil {
			rows.Close()
			return err
		}
		a.running = state == "running"
		list = append(list, a)
	}
	rows.Close()

	step(out, "App data (%d apps)", len(list))
	var routes []edge.Route
	for _, a := range list {
		if a.domain != "" {
			routes = append(routes, edge.Route{Host: a.domain, App: a.name, Port: a.port})
		}
		if err := restoreAppData(ctx, cl, sc.Prefix, kit.HostID, a.name, lay, work, out); err != nil {
			return fmt.Errorf("app %s: %w", a.name, err)
		}
	}

	step(out, "Public IP addresses")
	if o.domain != "" {
		cfg.Edge.DashboardDomain = o.domain
	} else if cfg.Edge.DashboardDomain == "" {
		cfg.Edge.DashboardDomain = kit.Dashboard
	}
	if err := edge.ValidHostname(cfg.Edge.DashboardDomain); err != nil {
		return fmt.Errorf("dashboard domain: %w (pass --domain)", err)
	}
	v4, v6, err := chooseIPs(ctx, cfg, o, p, out)
	if err != nil {
		return err
	}
	if err := cfg.Save(o.cfgPath); err != nil {
		return err
	}
	fmt.Fprintf(out, "config: %s (dashboard %s)\n", o.cfgPath, cfg.Edge.DashboardDomain)

	step(out, "Pointing DNS at this server, new certificates and AOP")
	if _, ok, _ := st.GetSetting(ctx, edge.SettingCFToken); !ok {
		fmt.Fprintln(out, "warning: the restored settings have no Cloudflare token; set it in the dashboard, then sync")
	} else if err := edgeSetup(ctx, cfg, st, box, v4, v6, "", routes, out); err != nil {
		return err
	}
	st.Close()
	if err := startService(o, out); err != nil {
		return err
	}

	if !o.noStart && !o.noDeploy {
		step(out, "Deploying the apps that were running")
		c := newCtlClient(o.socket)
		for _, a := range list {
			if !a.running {
				fmt.Fprintf(out, "%s: was stopped; deploy it from the dashboard when you need it\n", a.name)
				continue
			}
			var res map[string]int64
			if err := c.do(http.MethodPost, "/v1/apps/"+a.name+"/deploy", nil, &res); err != nil {
				fmt.Fprintf(out, "%s: deploy not queued: %v\n", a.name, err)
				continue
			}
			fmt.Fprintf(out, "%s: deployment #%d queued (follow: dootd ctl deployments %s)\n", a.name, res["id"], a.name)
		}
	}
	fmt.Fprintf(out, "\nDone. https://%s works again once DNS has updated; sign in with your usual password.\n", cfg.Edge.DashboardDomain)
	fmt.Fprintf(out, "This server now uploads backups as host %s. Shut the old server down if it still runs, so the two do not write to the same place.\n", kit.HostID)
	return nil
}

func restoreSelfDB(ctx context.Context, cl *s3.Client, key, work, dbPath string) error {
	gz := filepath.Join(work, "dootd.db.zst")
	dctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := cl.GetFile(dctx, key, gz); err != nil {
		return err
	}
	plain := filepath.Join(work, "dootd.db")
	if err := backup.Decompress(gz, plain); err != nil {
		return fmt.Errorf("decompress: %w", err)
	}
	if err := backup.QuickCheck(ctx, plain); err != nil {
		return err
	}
	for _, s := range []string{"-wal", "-shm", "-journal"} {
		os.Remove(dbPath + s)
	}
	if err := os.Rename(plain, dbPath); err != nil {
		return err
	}
	return os.Chmod(dbPath, 0o600)
}

// resetForNewServer drops state that only made sense on the old machine:
// release directories (rebuilt by deploying), local backup copies,
// sessions, and the marker of the last dootd.db backup (so a fresh one is
// taken at the first start).
func resetForNewServer(ctx context.Context, st *store.Store) error {
	tx, err := st.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM releases`,
		`DELETE FROM sessions`,
		`UPDATE backups SET local_path = ''`,
		`DELETE FROM backups WHERE object_key = '' AND local_path = ''`,
		`DELETE FROM settings WHERE key = 'dootd_db_backup_at'`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return tx.Commit()
}

// restoreAppData creates the app's user and directories and unpacks its
// newest backup into DATA_DIR.
func restoreAppData(ctx context.Context, cl *s3.Client, prefix, hostID, name string, lay layout.Layout, work string, out io.Writer) error {
	u, err := users.Ensure(name)
	if err != nil {
		return err
	}
	if err := lay.EnsureApp(name, u); err != nil {
		return err
	}
	lctx, cancel := context.WithTimeout(ctx, time.Minute)
	objs, err := cl.List(lctx, backup.KeyPrefix(prefix, hostID, name))
	cancel()
	if err != nil {
		return err
	}
	o, ok := backup.Newest(objs)
	if !ok {
		fmt.Fprintf(out, "%s: no backup in the bucket; it starts with an empty DATA_DIR\n", name)
		return nil
	}
	dst := filepath.Join(work, name+".tar.zst")
	dctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err := cl.GetFile(dctx, o.Key, dst); err != nil {
		return err
	}
	files, err := backup.UnpackInto(ctx, dst, work, lay.DataDir(name), name, int(u.UID), int(u.GID))
	if err != nil {
		return fmt.Errorf("backup %s failed verification: %w", o.Key, err)
	}
	fmt.Fprintf(out, "%s: restored %s from %s\n", name, strings.Join(files, ", "), filepath.Base(o.Key))
	return nil
}
