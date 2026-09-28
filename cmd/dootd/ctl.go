package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/sumitwaani2/dootd/internal/backup"
	"github.com/sumitwaani2/dootd/internal/control"
	"github.com/sumitwaani2/dootd/internal/deployer"
	"github.com/sumitwaani2/dootd/internal/edge"
)

const ctlUsage = `dootd ctl - control a running dootd over its local socket (run as root)

Usage:
  dootd ctl [--socket PATH] <command> [args]

Commands:
  status                        List apps and their state
  deploy <app> [--detach]       Build the branch HEAD and deploy it (streams the build log)
  rollback <app> <release>      Switch back to a kept release (no build)
  releases <app>                List kept releases
  deployments <app>             Show deploy history
  logs <app> [-f] [-n N]        Show (and follow) app logs
  start|stop|restart <app>      Control an app
  backup <app>                  Back up the app's SQLite databases now
  backups <app>                 List backups
  restore <app> <backup id>     Restore a backup (stops and restarts the app)
  github-token                  Read a GitHub token from stdin and store it encrypted
                                (empty input removes it)
  admin set-password [--email E]  Set the dashboard admin email and password
                                (prompts; or reads the password from stdin when not a terminal)
  cloudflare-token              Read a Cloudflare API token from stdin, verify and store it encrypted
  edge                          Show TLS, DNS, AOP and Cloudflare status
  edge sync                     Sync DNS records, certificates and AOP with Cloudflare now
  edge set-strict <zone>        Set a zone's SSL/TLS mode to Full (strict)
`

type ctlClient struct {
	http *http.Client
}

func newCtlClient(socket string) *ctlClient {
	return &ctlClient{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}}
}

func (c *ctlClient) do(method, path string, body io.Reader, out any) error {
	req, err := http.NewRequest(method, "http://dootd"+path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ne *net.OpError
		if errors.As(err, &ne) {
			return fmt.Errorf("cannot reach dootd (is it running, and are you root?): %v", ne.Err)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if w, ok := out.(io.Writer); ok {
		_, err = io.Copy(w, resp.Body)
		return err
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func runCtl(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ctl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", control.DefaultSocket, "control socket path")
	fs.Usage = func() { fmt.Fprint(stderr, ctlUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprint(stderr, ctlUsage)
		return 2
	}
	c := newCtlClient(*socket)
	cmd, rest := rest[0], rest[1:]
	err := func() error {
		switch cmd {
		case "status":
			return ctlStatus(c, stdout)
		case "deploy":
			return ctlDeploy(c, rest, stdout)
		case "rollback":
			return ctlRollback(c, rest, stdout)
		case "releases":
			return ctlReleases(c, rest, stdout)
		case "deployments":
			return ctlDeployments(c, rest, stdout)
		case "logs":
			return ctlLogs(c, rest, stdout)
		case "start", "stop", "restart":
			if len(rest) != 1 {
				return fmt.Errorf("usage: dootd ctl %s <app>", cmd)
			}
			var res map[string]string
			if err := c.do(http.MethodPost, "/v1/apps/"+rest[0]+"/"+cmd, nil, &res); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s: %s\n", rest[0], res["state"])
			return nil
		case "github-token":
			tok, err := io.ReadAll(io.LimitReader(bufio.NewReader(os.Stdin), 4096))
			if err != nil {
				return err
			}
			var res map[string]string
			if err := c.do(http.MethodPut, "/v1/settings/github-token", strings.NewReader(string(tok)), &res); err != nil {
				return err
			}
			fmt.Fprintln(stdout, res["result"])
			if res["warning"] != "" {
				fmt.Fprintln(stdout, "warning:", res["warning"])
			}
			return nil
		case "cloudflare-token":
			tok, err := io.ReadAll(io.LimitReader(bufio.NewReader(os.Stdin), 4096))
			if err != nil {
				return err
			}
			var res struct {
				Result string   `json:"result"`
				Zones  []string `json:"zones"`
			}
			if err := c.do(http.MethodPut, "/v1/settings/cloudflare-token", strings.NewReader(string(tok)), &res); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s; zones readable by the token: %s\n", res.Result, strings.Join(res.Zones, ", "))
			return nil
		case "edge":
			return ctlEdge(c, rest, stdout)
		case "backup", "backups", "restore":
			return ctlBackup(c, cmd, rest, stdout)
		case "admin":
			if len(rest) == 0 || rest[0] != "set-password" {
				return errors.New("usage: dootd ctl admin set-password [--email you@example.com]")
			}
			return ctlSetPassword(c, rest[1:], stdout, stderr)
		case "help", "-h", "--help":
			fmt.Fprint(stdout, ctlUsage)
			return nil
		}
		return fmt.Errorf("unknown ctl command %q\n\n%s", cmd, ctlUsage)
	}()
	if err != nil {
		fmt.Fprintln(stderr, "dootd ctl:", err)
		return 1
	}
	return 0
}

func ctlStatus(c *ctlClient, out io.Writer) error {
	var apps []control.AppView
	if err := c.do(http.MethodGet, "/v1/apps", nil, &apps); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "APP\tSTATE\tDOMAIN\tPORT\tRELEASE\tPID\tMEM\tRESTARTS\tREQS\tSOURCE\tNOTE")
	for _, a := range apps {
		src := "prebuilt"
		if a.Deployable {
			src = a.Repo + "@" + a.Branch
			if a.Subdir != "" {
				src += ":" + a.Subdir
			}
		}
		note := a.LastError
		if a.Pending != 0 {
			note = fmt.Sprintf("deployment #%d in progress", a.Pending)
		}
		rel := a.Release
		if rel == "" {
			rel = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%d\t%dM\t%d\t%d\t%s\t%s\n", a.Name, a.State, orDash(a.Domain), a.Port, rel, a.PID, a.MemBytes>>20, a.Restarts, a.Requests, src, note)
	}
	return tw.Flush()
}

func ctlDeploy(c *ctlClient, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	detach := fs.Bool("detach", false, "queue the deploy and return immediately")
	app, err := oneArg(fs, args, "usage: dootd ctl deploy <app> [--detach]")
	if err != nil {
		return err
	}
	var res map[string]int64
	if err := c.do(http.MethodPost, "/v1/apps/"+app+"/deploy", nil, &res); err != nil {
		return err
	}
	return followDeployment(c, res["id"], *detach, out)
}

func ctlRollback(c *ctlClient, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	detach := fs.Bool("detach", false, "queue the rollback and return immediately")
	if err := fs.Parse(reorder(args)); err != nil || fs.NArg() != 2 {
		return errors.New("usage: dootd ctl rollback <app> <release>")
	}
	body, _ := json.Marshal(map[string]string{"release": fs.Arg(1)})
	var res map[string]int64
	if err := c.do(http.MethodPost, "/v1/apps/"+fs.Arg(0)+"/rollback", strings.NewReader(string(body)), &res); err != nil {
		return err
	}
	return followDeployment(c, res["id"], *detach, out)
}

func followDeployment(c *ctlClient, id int64, detach bool, out io.Writer) error {
	fmt.Fprintf(out, "deployment #%d queued\n", id)
	if detach {
		return nil
	}
	if err := c.do(http.MethodGet, fmt.Sprintf("/v1/deployments/%d/log?follow=1", id), nil, out); err != nil {
		return err
	}
	var d deployer.Deployment
	if err := c.do(http.MethodGet, fmt.Sprintf("/v1/deployments/%d", id), nil, &d); err != nil {
		return err
	}
	if d.Status != deployer.StatusSucceeded {
		return fmt.Errorf("deployment #%d %s: %s", id, d.Status, d.Error)
	}
	fmt.Fprintf(out, "deployment #%d succeeded: %s is now serving release %s\n", id, d.App, d.ReleaseID)
	return nil
}

func ctlReleases(c *ctlClient, args []string, out io.Writer) error {
	app, err := oneArg(flag.NewFlagSet("releases", flag.ContinueOnError), args, "usage: dootd ctl releases <app>")
	if err != nil {
		return err
	}
	var rs []deployer.Release
	if err := c.do(http.MethodGet, "/v1/apps/"+app+"/releases", nil, &rs); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RELEASE\tCURRENT\tCOMMIT\tZIG\tBUILT\tMESSAGE")
	for _, r := range rs {
		cur := ""
		if r.Current {
			cur = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, cur, short(r.GitSHA), r.ZigVersion, r.CreatedAt.Local().Format(time.DateTime), r.Subject)
	}
	return tw.Flush()
}

func ctlDeployments(c *ctlClient, args []string, out io.Writer) error {
	app, err := oneArg(flag.NewFlagSet("deployments", flag.ContinueOnError), args, "usage: dootd ctl deployments <app>")
	if err != nil {
		return err
	}
	var ds []deployer.Deployment
	if err := c.do(http.MethodGet, "/v1/apps/"+app+"/deployments", nil, &ds); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tSTATUS\tRELEASE\tSTARTED\tTOOK\tERROR")
	for _, d := range ds {
		took := "-"
		if !d.FinishedAt.IsZero() && !d.StartedAt.IsZero() {
			took = d.FinishedAt.Sub(d.StartedAt).String()
		}
		started := "-"
		if !d.StartedAt.IsZero() {
			started = d.StartedAt.Local().Format(time.DateTime)
		}
		errMsg := d.Error
		if len(errMsg) > 100 {
			errMsg = errMsg[:100] + "…"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", d.ID, d.Kind, d.Status, orDash(d.ReleaseID), started, took, errMsg)
	}
	return tw.Flush()
}

func ctlLogs(c *ctlClient, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fs.Bool("f", false, "follow")
	n := fs.Int("n", 100, "number of recent lines")
	app, err := oneArg(fs, args, "usage: dootd ctl logs <app> [-f] [-n N]")
	if err != nil {
		return err
	}
	q := fmt.Sprintf("?n=%d", *n)
	if *follow {
		q += "&follow=1"
	}
	return c.do(http.MethodGet, "/v1/apps/"+app+"/logs"+q, nil, out)
}

// oneArg parses flags (allowed before or after the positional arg) and
// returns the single positional argument.
func oneArg(fs *flag.FlagSet, args []string, usage string) (string, error) {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(reorder(args)); err != nil || fs.NArg() != 1 {
		return "", errors.New(usage)
	}
	return fs.Arg(0), nil
}

// reorder moves flags before positional arguments so both orders work.
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if a == "-n" && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func ctlEdge(c *ctlClient, args []string, out io.Writer) error {
	var st edge.Status
	switch {
	case len(args) == 0:
		if err := c.do(http.MethodGet, "/v1/edge", nil, &st); err != nil {
			return err
		}
		printEdge(out, st)
		return nil
	case args[0] == "sync" && len(args) == 1:
		var res struct {
			Status edge.Status `json:"status"`
			Error  string      `json:"error"`
		}
		if err := c.do(http.MethodPost, "/v1/edge/sync", nil, &res); err != nil {
			return err
		}
		printEdge(out, res.Status)
		if res.Error != "" {
			return errors.New("sync finished with errors:\n" + res.Error)
		}
		fmt.Fprintln(out, "sync OK")
		return nil
	case args[0] == "set-strict" && len(args) == 2:
		var res map[string]string
		if err := c.do(http.MethodPost, "/v1/edge/zones/"+args[1]+"/ssl-strict", nil, &res); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: %s\n", args[1], res["result"])
		return nil
	}
	return errors.New("usage: dootd ctl edge [sync | set-strict <zone>]")
}

func printEdge(out io.Writer, st edge.Status) {
	fmt.Fprintf(out, "listen %s   dashboard %s   cloudflare token %s\n", st.Listen, orDash(st.Dashboard), map[bool]string{true: "set", false: "NOT SET"}[st.TokenSet])
	fmt.Fprintf(out, "public IPv4 %s   IPv6 %s   Cloudflare ranges %d (%s)   rejected connections %d\n",
		orDash(st.PublicIPv4), orDash(st.PublicIPv6), st.IPRanges, st.IPSource, st.Rejected)
	if !st.LastSync.IsZero() {
		fmt.Fprintf(out, "last sync %s\n", st.LastSync.Local().Format(time.DateTime))
	}
	fmt.Fprintln(out)
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tAPP\tZONE\tDNS\tCERT UNTIL\tERROR")
	for _, h := range st.Hosts {
		until := "-"
		if !h.CertUntil.IsZero() {
			until = h.CertUntil.Format(time.DateOnly)
		}
		app := h.App
		if app == "" {
			app = "(dashboard)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", h.Host, app, orDash(h.Zone), orDash(h.DNS), until, h.Error)
	}
	tw.Flush()
	if len(st.Zones) > 0 {
		fmt.Fprintln(out)
		tw = tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ZONE\tSSL MODE\tAOP\tWARNING / ERROR")
		for _, z := range st.Zones {
			note := z.Warning
			if z.Error != "" {
				note = strings.TrimSpace(note + " " + z.Error)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", z.Name, orDash(z.SSLMode), z.AOP, note)
		}
		tw.Flush()
	}
}

func ctlSetPassword(c *ctlClient, args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("set-password", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	email := fs.String("email", "", "admin email (default: keep the current one)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: dootd ctl admin set-password [--email you@example.com]")
	}
	var pw string
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		fmt.Fprint(errOut, "New password (at least 12 characters): ")
		a, err := term.ReadPassword(fd)
		fmt.Fprintln(errOut)
		if err != nil {
			return err
		}
		fmt.Fprint(errOut, "Repeat password: ")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(errOut)
		if err != nil {
			return err
		}
		if string(a) != string(b) {
			return errors.New("passwords do not match")
		}
		pw = string(a)
	} else {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		if err != nil {
			return err
		}
		pw = strings.TrimRight(string(b), "\r\n")
	}
	body, _ := json.Marshal(map[string]string{"email": *email, "password": pw})
	var res map[string]string
	if err := c.do(http.MethodPut, "/v1/admin", strings.NewReader(string(body)), &res); err != nil {
		return err
	}
	fmt.Fprintln(out, res["result"])
	return nil
}

func ctlBackup(c *ctlClient, cmd string, args []string, out io.Writer) error {
	switch {
	case cmd == "backup" && len(args) == 1:
		var b backup.Backup
		if err := c.do(http.MethodPost, "/v1/apps/"+args[0]+"/backups", nil, &b); err != nil {
			return err
		}
		where := "this server only"
		if b.Uploaded() {
			where = "bucket + server"
		}
		fmt.Fprintf(out, "backup #%d: %d database(s), %d bytes, %s\n", b.ID, b.Files, b.Size, where)
		if b.Error != "" {
			fmt.Fprintln(out, "warning:", b.Error)
		}
		return nil
	case cmd == "backups" && len(args) == 1:
		var bs []backup.Backup
		if err := c.do(http.MethodGet, "/v1/apps/"+args[0]+"/backups", nil, &bs); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tTAKEN\tKIND\tSTATUS\tSIZE\tBUCKET\tSERVER\tERROR")
		for _, b := range bs {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%d\t%v\t%v\t%s\n", b.ID, b.CreatedAt.Local().Format(time.DateTime), b.Kind, b.Status, b.Size, b.Uploaded(), b.LocalPath != "", b.Error)
		}
		return tw.Flush()
	case cmd == "restore" && len(args) == 2:
		var id int64
		if _, err := fmt.Sscan(args[1], &id); err != nil {
			return errors.New("backup id must be a number")
		}
		body, _ := json.Marshal(map[string]int64{"id": id})
		var res backup.RestoreResult
		if err := c.do(http.MethodPost, "/v1/apps/"+args[0]+"/restore", strings.NewReader(string(body)), &res); err != nil {
			return err
		}
		fmt.Fprintf(out, "restored %s", strings.Join(res.Files, ", "))
		if res.MovedTo != "" {
			fmt.Fprintf(out, "; previous databases moved to %s", res.MovedTo)
		}
		fmt.Fprintln(out)
		if res.StartError != "" {
			return errors.New("the app did not start again: " + res.StartError)
		}
		return nil
	}
	return fmt.Errorf("usage: dootd ctl %s <app>%s", cmd, map[bool]string{true: " <backup id>"}[cmd == "restore"])
}
