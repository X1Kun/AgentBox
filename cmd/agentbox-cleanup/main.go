// agentbox-cleanup is a one-shot administrator tool; it does not use API user
// tokens, impersonate tenants or expose a public deletion endpoint.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/X1Kun/AgentBox/internal/session"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
	flags := flag.NewFlagSet("agentbox-cleanup", flag.ContinueOnError)
	var cfg session.Config
	var opts session.CleanupOptions
	var kubeconfig, principal, reportPath string
	var timeout, requestTimeout time.Duration
	flags.StringVar(&kubeconfig, "kubeconfig", "", "explicit administrator kubeconfig; never loads default credentials")
	flags.StringVar(&cfg.Namespace, "namespace", "", "fixed workload namespace")
	flags.StringVar(&cfg.Pool, "pool", "", "fixed pool name")
	flags.StringVar(&cfg.Instance, "instance", "", "API installation identity")
	flags.StringVar(&principal, "principal", "", "owner principal ID; not its bearer token")
	flags.StringVar(&reportPath, "report", "", "new private JSON report path; must not already exist")
	flags.StringVar(&opts.QuotaName, "quota-name", "agentbox-api-budget", "record ResourceQuota name; empty disables quota inspection")
	flags.DurationVar(&opts.Retention, "retention", 24*time.Hour, "retention since first confirmed API cleanup, 1s..8760h")
	flags.IntVar(&opts.MaxRecords, "max-records", 100, "maximum scoped records per invocation, 1..1000")
	flags.BoolVar(&opts.Apply, "apply", false, "persist closure observations and delete eligible records; default is read-only preview")
	flags.DurationVar(&timeout, "timeout", 2*time.Minute, "total invocation budget, 1s..30m")
	flags.DurationVar(&requestTimeout, "request-timeout", 5*time.Second, "individual Kubernetes request budget, 1s..30s")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || kubeconfig == "" || reportPath == "" || !session.ValidPrincipal(principal) || timeout < time.Second || timeout > 30*time.Minute || requestTimeout < time.Second || requestTimeout > 30*time.Second {
		return errors.New("explicit kubeconfig, report and valid scope/budgets are required")
	}
	if opts.Retention < time.Second || opts.Retention > 365*24*time.Hour || opts.Retention%time.Second != 0 || opts.MaxRecords < 1 || opts.MaxRecords > 1000 {
		return errors.New("invalid retention or record limit")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return errors.New("unable to load cleanup credentials")
	}
	config.Timeout = requestTimeout
	config.QPS = 10
	config.Burst = 20
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return errors.New("unable to configure cleanup client")
	}
	service, err := session.New(client, cfg)
	if err != nil {
		return err
	}
	// Reserve an exclusive path so an invocation cannot overwrite its predecessor.
	reserved, err := os.OpenFile(reportPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("report must be a new writable path")
	}
	if err = reserved.Close(); err != nil {
		return errors.New("unable to reserve report")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	report, sweepErr := service.Sweep(ctx, principal, opts, func(r session.CleanupReport) error { return writeReport(reportPath, r) })
	if report.Quota != nil && report.Quota.Pressure != "normal" {
		fmt.Fprintf(os.Stderr, "record quota pressure: %s\n", report.Quota.Pressure)
	}
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		return errors.New("unable to write cleanup summary")
	}
	return sweepErr
}

// Atomic replacement plus file and directory sync make the pre-mutation intent
// durable. A failed write stops the sweep, leaving the last valid intent intact.
func writeReport(path string, report session.CleanupReport) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".agentbox-cleanup-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(report); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
