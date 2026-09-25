//go:build darwin

package macos

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
	"github.com/ebitengine/purego"
)

const cfUTF8 = 0x08000100

type preferencesAPI struct {
	newString  func(uintptr, string, uint32) uintptr
	release    func(uintptr)
	sync       func(uintptr) bool
	forced     func(uintptr, uintptr) bool
	copyValue  func(uintptr, uintptr) uintptr
	typeID     func(uintptr) uintptr
	stringType func() uintptr
	getCString func(uintptr, *byte, int64, uint32) bool
}

// CoreFoundation resolves device and user profiles without depending on their on-disk layout.
var loadPreferences = sync.OnceValues(func() (*preferencesAPI, error) {
	lib, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, fmt.Errorf("load managed preferences: %w", err)
	}
	api := &preferencesAPI{}
	for name, fn := range map[string]any{
		"CFStringCreateWithCString":     &api.newString,
		"CFRelease":                     &api.release,
		"CFPreferencesAppSynchronize":   &api.sync,
		"CFPreferencesAppValueIsForced": &api.forced,
		"CFPreferencesCopyAppValue":     &api.copyValue,
		"CFGetTypeID":                   &api.typeID,
		"CFStringGetTypeID":             &api.stringType,
		"CFStringGetCString":            &api.getCString,
	} {
		addr, err := purego.Dlsym(lib, name)
		if err != nil {
			return nil, fmt.Errorf("load managed preference function %s: %w", name, err)
		}
		purego.RegisterFunc(fn, addr)
	}
	return api, nil
})

func ManagedEnrollment() (server, grant string, err error) {
	if os.Geteuid() == 0 {
		return "", "", common.ErrRoot
	}
	api, err := loadPreferences()
	if err != nil {
		return "", "", err
	}
	domain := api.newString(0, bundleIdentifier, cfUTF8)
	defer api.release(domain)
	api.sync(domain)
	server, err = api.read(domain, "Server")
	if err != nil {
		return "", "", err
	}
	grant, err = api.read(domain, "Grant")
	if err != nil {
		return "", "", err
	}
	if (server == "") != (grant == "") {
		return "", "", fmt.Errorf("managed preferences %s require both Server and Grant", bundleIdentifier)
	}
	return server, grant, nil
}

func (api *preferencesAPI) read(domain uintptr, key string) (string, error) {
	cfkey := api.newString(0, key, cfUTF8)
	defer api.release(cfkey)
	if !api.forced(cfkey, domain) {
		return "", nil
	}
	value := api.copyValue(cfkey, domain)
	if value == 0 {
		return "", nil
	}
	defer api.release(value)
	if api.typeID(value) != api.stringType() {
		return "", fmt.Errorf("managed preference %s must be a string", key)
	}
	buf := make([]byte, 8192)
	if !api.getCString(value, &buf[0], int64(len(buf)), cfUTF8) {
		return "", fmt.Errorf("managed preference %s exceeds its size limit", key)
	}
	return strings.TrimSpace(string(buf[:bytes.IndexByte(buf, 0)])), nil
}
