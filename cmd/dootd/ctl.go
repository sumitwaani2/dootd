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

	"github.com/sumitwaani2/dootd/internal/control"
	"github.com/sumitwaani2/dootd/internal/deployer"
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
  github-token                  Read a GitHub token from stdin and store it encrypted
                                (empty input removes it)
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
	fmt.Fprintln(tw, "APP\tSTATE\tPORT\tRELEASE\tPID\tMEM\tRESTARTS\tSOURCE\tNOTE")
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
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%d\t%dM\t%d\t%s\t%s\n", a.Name, a.State, a.Port, rel, a.PID, a.MemBytes>>20, a.Restarts, src, note)
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
