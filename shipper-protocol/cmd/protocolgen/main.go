// Command protocolgen renders the rulebook table in PROTOCOL.md from authority.json, between the
// <!-- rulebook:begin --> and <!-- rulebook:end --> markers. Run it from the protocol module root:
// `go run ./cmd/protocolgen` rewrites the table and `go run ./cmd/protocolgen -check` checks it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

type field struct {
	Field     string `json:"field"`
	Default   string `json:"default"`
	ServerMay string `json:"server_may"`
	Note      string `json:"note"`
}

type rulebook struct {
	Comment     string            `json:"comment"`
	Classes     map[string]string `json:"classes"`
	Fields      []field           `json:"fields"`
	NotSettable []string          `json:"not_settable"`
}

const (
	beginMarker = "<!-- rulebook:begin -->"
	endMarker   = "<!-- rulebook:end -->"
)

var classLabels = map[string]string{
	"set":      "set",
	"union":    "add only",
	"off-only": "turn off only",
	"refused":  "refused",
	"envelope": "served document only",
	"ignored":  "read and ignored",
}

func render(rb rulebook) (string, error) {
	var b strings.Builder
	b.WriteString("| Field | Default | Server may | Enforcement |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	for _, f := range rb.Fields {
		label, ok := classLabels[f.ServerMay]
		if !ok {
			return "", fmt.Errorf("field %q: unknown class %q", f.Field, f.ServerMay)
		}
		if _, declared := rb.Classes[f.ServerMay]; !declared {
			return "", fmt.Errorf("field %q: class %q is not declared in classes", f.Field, f.ServerMay)
		}
		b.WriteString(fmt.Sprintf("| `%s` | %s | **%s** | %s |\n",
			f.Field, cell(f.Default), label, cell(f.Note)))
	}
	b.WriteString("\nNot settable from any config layer:\n\n")
	for _, n := range rb.NotSettable {
		b.WriteString("- " + n + "\n")
	}
	return b.String(), nil
}

func cell(s string) string {
	if s == "" {
		return "—"
	}
	return strings.ReplaceAll(s, "|", "\\|")
}

func main() {
	check := flag.Bool("check", false, "verify PROTOCOL.md is current instead of rewriting it")
	authorityPath := flag.String("authority", "authority.json", "path to authority.json")
	docPath := flag.String("doc", "PROTOCOL.md", "path to PROTOCOL.md")
	flag.Parse()

	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
		os.Exit(1)
	}

	rawAuthority, err := os.ReadFile(*authorityPath)
	if err != nil {
		fail("%v", err)
	}
	var rb rulebook
	if err := json.Unmarshal(rawAuthority, &rb); err != nil {
		fail("parse %s: %v", *authorityPath, err)
	}
	table, err := render(rb)
	if err != nil {
		fail("%v", err)
	}

	rawDoc, err := os.ReadFile(*docPath)
	if err != nil {
		fail("%v", err)
	}
	docText := string(rawDoc)
	begin := strings.Index(docText, beginMarker)
	end := strings.Index(docText, endMarker)
	if begin < 0 || end < 0 || end < begin {
		fail("%s does not carry the %s / %s markers", *docPath, beginMarker, endMarker)
	}

	current := docText[begin+len(beginMarker) : end]
	want := "\n" + table
	if *check {
		if current != want {
			fail("the rulebook table in %s is stale: run `go run ./cmd/protocolgen` from the module root after editing authority.json", *docPath)
		}
		return
	}
	updated := docText[:begin+len(beginMarker)] + want + docText[end:]
	if err := os.WriteFile(*docPath, []byte(updated), 0o644); err != nil {
		fail("%v", err)
	}
}
