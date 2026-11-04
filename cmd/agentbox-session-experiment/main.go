package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	experiment "github.com/X1Kun/AgentBox/internal/experiment/session"
	client "github.com/X1Kun/AgentBox/internal/session/client"
	"io"
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
	var config, ledger, base, ca, users string
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
	summary, err := experiment.Run(ctx, c, actors, func(e experiment.Event) error { return enc.Encode(e) })
	if syncErr := out.Sync(); syncErr != nil {
		return syncErr
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(summary)
}
