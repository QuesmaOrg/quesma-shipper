package sources

import (
	"context"
	"fmt"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Noninteractive native reads preserve CGO_ENABLED=0 and the no-subprocess boundary.
func readAccountKeychain(ctx context.Context, service string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, fmt.Errorf("CoreFoundation unavailable")
	}
	defer purego.Dlclose(cf)
	sec, err := purego.Dlopen("/System/Library/Frameworks/Security.framework/Security", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, fmt.Errorf("Security framework unavailable")
	}
	defer purego.Dlclose(sec)
	var createString func(uintptr, string, uint32) uintptr
	var createDict func(uintptr, int64, uintptr, uintptr) uintptr
	var setValue func(uintptr, uintptr, uintptr)
	var release func(uintptr)
	var getLength func(uintptr) int64
	var getBytes func(uintptr) *byte
	var symbolAddress func(uintptr, string) *uintptr
	var copyMatching func(uintptr, *uintptr) int32
	purego.RegisterLibFunc(&createString, cf, "CFStringCreateWithCString")
	purego.RegisterLibFunc(&createDict, cf, "CFDictionaryCreateMutable")
	purego.RegisterLibFunc(&setValue, cf, "CFDictionarySetValue")
	purego.RegisterLibFunc(&release, cf, "CFRelease")
	purego.RegisterLibFunc(&getLength, cf, "CFDataGetLength")
	purego.RegisterLibFunc(&getBytes, cf, "CFDataGetBytePtr")
	purego.RegisterLibFunc(&copyMatching, sec, "SecItemCopyMatching")
	purego.RegisterLibFunc(&symbolAddress, cf, "dlsym")
	constant := func(handle uintptr, name string) (uintptr, error) {
		symbol := symbolAddress(handle, name)
		if symbol == nil {
			return 0, fmt.Errorf("Keychain constant unavailable")
		}
		return *symbol, nil
	}
	dict := createDict(0, 0, 0, 0)
	if dict == 0 {
		return nil, fmt.Errorf("Keychain query allocation failed")
	}
	defer release(dict)
	for key, value := range map[string]string{
		"kSecClass":               "kSecClassGenericPassword",
		"kSecMatchLimit":          "kSecMatchLimitOne",
		"kSecUseAuthenticationUI": "kSecUseAuthenticationUIFail",
	} {
		k, err := constant(sec, key)
		if err != nil {
			return nil, err
		}
		v, err := constant(sec, value)
		if err != nil {
			return nil, err
		}
		setValue(dict, k, v)
	}
	k, err := constant(sec, "kSecAttrService")
	if err != nil {
		return nil, err
	}
	name := createString(0, service, 0x08000100)
	if name == 0 {
		return nil, fmt.Errorf("Keychain service allocation failed")
	}
	defer release(name)
	setValue(dict, k, name)
	k, err = constant(sec, "kSecReturnData")
	if err != nil {
		return nil, err
	}
	yes, err := constant(cf, "kCFBooleanTrue")
	if err != nil {
		return nil, err
	}
	setValue(dict, k, yes)
	var data uintptr
	if status := copyMatching(dict, &data); status != 0 {
		return nil, fmt.Errorf("Keychain unavailable (%d)", status)
	}
	if data == 0 {
		return nil, fmt.Errorf("empty Keychain result")
	}
	defer release(data)
	size := getLength(data)
	if size <= 0 || size > accountResponseLimit {
		return nil, fmt.Errorf("invalid Keychain data size")
	}
	ptr := getBytes(data)
	if ptr == nil {
		return nil, fmt.Errorf("empty Keychain data")
	}
	return append([]byte(nil), unsafe.Slice(ptr, int(size))...), nil
}
