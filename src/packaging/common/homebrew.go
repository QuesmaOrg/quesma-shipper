package common

import (
	"path/filepath"
	"runtime"
)

const BrewUninstall = "brew uninstall --cask quesmaorg/tap/quesma-shipper"

// HomebrewCaskRoot recognizes the installed payload, including custom Homebrew prefixes.
func HomebrewCaskRoot(executable string) string {
	if !filepath.IsAbs(executable) || filepath.Base(executable) != "quesma-shipper" {
		return ""
	}
	root := filepath.Dir(filepath.Dir(executable))
	if filepath.Base(root) != "quesma-shipper" || filepath.Base(filepath.Dir(root)) != "Caskroom" {
		return ""
	}
	return root
}

func HomebrewManaged() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	exe, err := CurrentExecutable()
	return err == nil && HomebrewCaskRoot(exe) != ""
}
