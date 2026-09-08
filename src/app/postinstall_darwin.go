//go:build darwin

package app

import "github.com/QuesmaOrg/quesma-shipper/packaging"

// PostInstallPackage registers the program the macOS package has just installed.
func PostInstallPackage() (string, error) {
	_, err := packaging.PostInstall()
	return "", err
}
