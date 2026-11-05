package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	experiment "github.com/X1Kun/AgentBox/internal/experiment/session"
	client "github.com/X1Kun/AgentBox/internal/session/client"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	var config, ledger, base, ca, users, metricsListen string
	var healthLinger time.Duration
	flag.StringVar(&metricsListen, "health-listen", "", "optional loopback endpoint for trusted follow-up quality windows")
	flag.DurationVar(&healthLinger, "health-linger", 0, "keep final quality windows available after trace completion, at most 2m")
	flag.StringVar(&config, "config", "", "private JSON trace compiled from YAML")
	flag.StringVar(&ledger, "ledger", "", "new JSONL ledger")
	flag.StringVar(&base, "api-url", "", "explicit API URL")
	flag.StringVar(&ca, "api-ca-file", "", "trusted API CA")
	flag.StringVar(&users, "users-file", "", "private JSON actor-to-token map (0600)")
	flag.Parse()
	f, err := os.Open(config)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 2<<20))
	d.DisallowUnknownFields()
	var c experiment.Config
	if err = d.Decode(&c); err != nil {
		return err
	}
	if err = experiment.Validate(c); err != nil {
		return err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing config")
	}

	actors := experiment.Actors{}
	credentials := map[string]string{"": os.Getenv("AGENTBOX_API_TOKEN")}
	if users != "" {
		info, e := os.Stat(users)
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 65536 {
			return fmt.Errorf("users file must be private and bounded")
		}
		data, e := os.ReadFile(users)
		if e != nil {
			return e
		}
		credentials = map[string]string{}
		if e = json.Unmarshal(data, &credentials); e != nil {
			return e
		}
	}
	for name, token := range credentials {
		b, e := client.New(base, token, ca)
		if e != nil {
			return e
		}
		defer b.Close()
		actors[name] = b
	}
	if err = actors.Validate(c); err != nil {
		return err
	}
	out, err := os.OpenFile(ledger, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	enc := json.NewEncoder(out)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 200*time.Second)
	defer cancel()
	var quality *experiment.Quality
	if metricsListen != "" {
		host, _, e := net.SplitHostPort(metricsListen)
		if e != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || c.Health == nil || c.StartAt == "" || healthLinger < 0 || healthLinger > 2*time.Minute {
			return fmt.Errorf("health export requires loopback, explicit start, health contract and bounded linger")
		}
		start, e := time.Parse(time.RFC3339Nano, c.StartAt)
		if e != nil {
			return e
		}
		quality, e = experiment.NewQuality(c, *c.Health, start)
		if e != nil {
			return e
		}
		registry := prometheus.NewRegistry()
		registry.MustRegister(quality)
		listener, e := net.Listen("tcp", metricsListen)
		if e != nil {
			return e
		}
		server := &http.Server{Handler: promhttp.HandlerFor(registry, promhttp.HandlerOpts{Timeout: time.Second, MaxRequestsInFlight: 2}), ReadHeaderTimeout: time.Second, WriteTimeout: 2 * time.Second}
		defer server.Close()
		go func() {
			if e := server.Serve(listener); e != nil && e != http.ErrServerClosed {
				quality.Invalidate()
				cancel()
			}
		}()
	} else if c.Health != nil || healthLinger != 0 {
		return fmt.Errorf("health contract and linger require health-listen")
	}
	var arrival func(experiment.Event)
	if quality != nil {
		arrival = quality.Arrival
	}
	summary, err := experiment.RunObserved(ctx, c, actors, func(e experiment.Event) error {
		if e := enc.Encode(e); e != nil {
			if quality != nil {
				quality.Invalidate()
			}
			return e
		}
		if quality != nil {
			quality.Record(e)
		}
		return nil
	}, arrival)
	if syncErr := out.Sync(); syncErr != nil {
		return syncErr
	}
	if err != nil {
		if quality != nil {
			quality.Invalidate()
		}
		return err
	}
	if quality != nil && healthLinger > 0 {
		select {
		case <-time.After(healthLinger):
		case <-ctx.Done():
		}
	}
	return json.NewEncoder(os.Stdout).Encode(summary)
}
