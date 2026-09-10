package platform_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// moduleRoot is derived rather than a hardcoded relative depth, which a directory move silently misdirects.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// forEachModuleGoFile parses every non-test .go file in the module, skipping generated trees and
// nested modules, and hands each to fn. It is the shared walk behind the module-wide lints here.
func forEachModuleGoFile(t *testing.T, fn func(rel string, file *ast.File, fset *token.FileSet)) {
	t.Helper()
	root := moduleRoot(t)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "conformance", ".git":
				return fs.SkipDir
			}
			// A nested module is not in this module, the same boundary go list draws.
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return fs.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		fn(filepath.ToSlash(rel), file, fset)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// writeCapablePackages may open files for writing; everything else must route through safeio,
// which is what keeps "the shipper never writes inside an agent's store" true. Each entry earns
// its place by owning a specific durable artifact:
//
//	internal/engine/state.go    the fingerprint document
//	packaging/{common,macos,linux,windows} the program, app bundle, and supervision entries
//	internal/platform/auditlog  the append-only local log
//	internal/platform/crashjournal    the append-only crash journal
//	internal/sources/sqliteread scratch snapshots and cold copies of agent databases
//
// sqliteread weakens this lint, so it carries a compensating control rather than an exemption:
// it may write only under the scratch directory it is given, asserted by
// TestAReadNeverWritesBesideTheSource. The enricher packages that USE it stay off this list.
var writeCapablePackages = []string{
	"internal/platform/auditlog",
	"internal/platform/crashjournal",
	"internal/sources/sqliteread",
	"packaging/macos",
	"packaging/linux",
	"packaging/windows",
}

// writeCapableFiles is the FILE-granular half, for the merged platform package: a directory grant
// would extend write rights to memstat and buildinfo, which have none and must stay that way.
//
//	safeio.go   the sanctioned write path itself
//	engine/state.go  the fingerprint document (the engine dir itself has no blanket grant)
//	pause.go    the pause-state flag it must be able to set and clear
//	packaging/common/service.go  service state
//	packaging/common/remove.go  the installed standalone executable
//	packaging/common/selfupdate.go  the cross-supervisor self-update hop guard
var writeCapableFiles = []string{
	"internal/platform/safeio.go",
	"internal/engine/state.go",
	"packaging/common/service.go",
	"packaging/common/remove.go",
	"packaging/common/selfupdate.go",
	"internal/platform/pause.go",
	"internal/sources/ignore.go",
}

// bannedWrites are the os-level calls that create or truncate a file.
var bannedWrites = map[string]string{
	"Create":     "os.Create truncates and is not atomic",
	"WriteFile":  "os.WriteFile is not atomic: a crash mid-write leaves a torn file",
	"Mkdir":      "directory creation belongs with the artifact's owner",
	"MkdirAll":   "directory creation belongs with the artifact's owner",
	"Remove":     "deletion of local state belongs with the artifact's owner",
	"RemoveAll":  "deletion of local state belongs with the artifact's owner",
	"Rename":     "the atomic-rename commit point lives in safeio",
	"Truncate":   "truncation is never correct on a store the shipper reads",
	"Chmod":      "mode changes belong with the artifact's owner",
	"Symlink":    "the shipper never creates links",
	"Link":       "the shipper never creates links",
	"OpenFile":   "os.OpenFile can open for writing; use safeio",
	"CreateTemp": "temp files are safeio's business, so the suffix stays consistent",
}

// TestWritePathLint fails if a package outside the allow-list opens a file for writing: a lint, because Go cannot forbid an import.
func TestWritePathLint(t *testing.T) {
	var findings []string
	forEachModuleGoFile(t, func(rel string, file *ast.File, fset *token.FileSet) {
		if slices.Contains(writeCapablePackages, filepath.ToSlash(filepath.Dir(rel))) {
			return
		}
		if slices.Contains(writeCapableFiles, rel) {
			return
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok || pkgIdent.Name != "os" {
				return true
			}
			why, banned := bannedWrites[sel.Sel.Name]
			if !banned {
				return true
			}
			findings = append(findings, formatFinding(rel, fset.Position(call.Pos()).Line, sel.Sel.Name, why))
			return true
		})
	})

	for _, f := range findings {
		t.Error(f)
	}
	if len(findings) > 0 {
		t.Logf("packages permitted to write: %s", strings.Join(writeCapablePackages, ", "))
	}
}

func formatFinding(file string, line int, call, why string) string {
	return fmt.Sprintf("%s:%d: os.%s outside the write allow-list — %s", file, line, call, why)
}

// bannedExec are the calls that create a process. os/exec wraps os.StartProcess, so its import is
// banned outright; syscall's process spawners are named directly.
var bannedExec = map[string]map[string]bool{
	"os":      {"StartProcess": true},
	"syscall": {"Exec": true, "ForkExec": true},
}

// TestNoExecOutsidePackaging: os/exec reads as malware to an auditor, so the collector's data path
// never spawns a subprocess; packaging/ (service install, self-update re-exec) is the only exception.
func TestNoExecOutsidePackaging(t *testing.T) {
	forEachModuleGoFile(t, func(rel string, file *ast.File, fset *token.FileSet) {
		if rel == "packaging" || strings.HasPrefix(rel, "packaging/") {
			return
		}
		for _, imp := range file.Imports {
			if imp.Path.Value == `"os/exec"` {
				t.Errorf("%s:%d: imports os/exec: the data path must not spawn subprocesses (allowed only under packaging/)",
					rel, fset.Position(imp.Pos()).Line)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && bannedExec[pkg.Name][sel.Sel.Name] {
				t.Errorf("%s:%d: calls %s.%s: process creation is allowed only under packaging/",
					rel, fset.Position(sel.Pos()).Line, pkg.Name, sel.Sel.Name)
			}
			return true
		})
	})
}
