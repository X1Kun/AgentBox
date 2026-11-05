package main

import (
	"errors"
	"flag"
	"testing"
)

func TestFlagsAreIsolatedFromControllerRuntimeGlobals(t *testing.T) {
	if err := run([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatal(err)
	}
	if err := run([]string{"--kubeconfig", "/nonexistent", "--config", "/nonexistent", "--lease-namespace", "test"}); err == nil {
		t.Fatal("missing explicit policy was accepted")
	}
}
