package sources

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Env is the environment a resolution runs against, injectable so tests do not depend on the real machine.
type Env struct {
	Home   string
	Lookup func(string) (string, bool)
}

// OSEnv returns the real process environment.
func OSEnv() (Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Env{}, fmt.Errorf("config: resolve home directory: %w", err)
	}
	return Env{Home: home, Lookup: os.LookupEnv}, nil
}

// ExpandRoot expands a leading ~ and any $VAR references, then requires an absolute, clean result. The deny
// list must be applied to the value returned here and never to the template, since $CLAUDE_CONFIG_DIR is
// attacker-influenceable; a root naming an unset variable stays unresolved rather than becoming relative.
func (e Env) ExpandRoot(root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("empty root")
	}

	expanded, err := e.expandVars(root)
	if err != nil {
		return "", err
	}
	expanded = ExpandHome(expanded, e.Home)

	if !filepath.IsAbs(expanded) {
		return "", fmt.Errorf("root %q expanded to %q, which is not absolute", root, expanded)
	}
	return filepath.Clean(expanded), nil
}

// ErrUnsetVar marks a root that referenced an unset variable. Not a failure: the next candidate root is tried.
type ErrUnsetVar struct {
	Root string
	Var  string
}

func (e *ErrUnsetVar) Error() string {
	return fmt.Sprintf("root %q references unset variable %s", e.Root, e.Var)
}

func (e Env) expandVars(s string) (string, error) {
	var err error
	expanded := os.Expand(s, func(name string) string {
		value, ok := e.Lookup(name)
		if !ok || value == "" {
			err = &ErrUnsetVar{Root: s, Var: name}
		}
		return value
	})
	if err != nil {
		return "", err
	}
	return expanded, nil
}

// ExpandHome resolves a leading ~ against a home directory.
func ExpandHome(p, home string) string {
	if strings.HasPrefix(p, "~") {
		return home + strings.TrimPrefix(p, "~")
	}
	return p
}

// FirstExistingFile expands each candidate the way roots expand and returns the first that
// exists and is not a directory; empty means absent. The engine and doctor share it, so what
// doctor reports is what the enricher opens.
func (e Env) FirstExistingFile(candidates []string) string {
	for _, cand := range candidates {
		path, err := e.ExpandRoot(cand)
		if err != nil {
			continue
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}
