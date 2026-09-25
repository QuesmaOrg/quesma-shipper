package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNoArgumentsPrintVersionAndHelpWithoutServing(t *testing.T) {
	oldServe, oldVersion := serveCommand, version
	t.Cleanup(func() { serveCommand, version = oldServe, oldVersion })
	version = "1.2.3-test"
	serveCommand = func(context.Context, []string) error {
		t.Fatal("empty invocation entered server mode")
		return nil
	}

	var stdout bytes.Buffer
	if err := run(context.Background(), nil, &stdout); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fleet-manager 1.2.3-test", "fleet-manager --serve", "/admin/"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("output %q does not contain %q", stdout.String(), want)
		}
	}
}

func TestOnlyExplicitServeEntersServerMode(t *testing.T) {
	oldServe := serveCommand
	t.Cleanup(func() { serveCommand = oldServe })
	want := errors.New("entered server")
	serveCommand = func(_ context.Context, args []string) error {
		if len(args) != 2 || args[0] != "--provider" || args[1] != "aws" {
			t.Fatalf("serve arguments = %q", args)
		}
		return want
	}

	if err := run(context.Background(), []string{"serve"}, &bytes.Buffer{}); err == nil {
		t.Fatal("legacy serve command was accepted")
	}
	if err := run(context.Background(), []string{"init"}, &bytes.Buffer{}); err == nil {
		t.Fatal("legacy administrative command was accepted")
	}
	if err := run(context.Background(), []string{"--serve", "--provider", "aws"}, &bytes.Buffer{}); !errors.Is(err, want) {
		t.Fatalf("explicit serve error = %v", err)
	}
}
