//go:build windows

package windows

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// maxACEs bounds the enumeration: GetAce reports the end of the list as an error, and a corrupt
// ACL must not become an unbounded loop.
const maxACEs = 4096

func verifyInstallDir(dir, installerSID string) error {
	aces, err := directoryACEs(dir)
	if err != nil {
		return err
	}
	writers := untrustedWriters(aces, installerSID)
	if len(writers) == 0 {
		return nil
	}
	return fmt.Errorf("supervise: %s can be modified by %s, who could then replace the program "+
		"Task Scheduler starts in your session: install into a directory only you and "+
		`administrators can change, such as the default %%LOCALAPPDATA%%\Programs\Quesma Shipper`,
		dir, strings.Join(describeTrustees(writers), ", "))
}

func directoryACEs(dir string) ([]ace, error) {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("supervise: read the permissions of %s: %w", dir, err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return nil, fmt.Errorf("supervise: read the permissions of %s: %w", dir, err)
	}
	// A NULL DACL is not an empty one: it grants every account full access.
	if acl == nil {
		return []ace{{SID: sidEveryone, Mask: genericAll, Allow: true}}, nil
	}

	var aces []ace
	for i := uint32(0); i < maxACEs; i++ {
		var entry *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &entry); err != nil {
			break
		}
		sid := (*windows.SID)(unsafe.Pointer(&entry.SidStart))
		aces = append(aces, ace{
			SID:         sid.String(),
			Mask:        uint32(entry.Mask),
			Allow:       entry.Header.AceType == accessAllowedACEType,
			InheritOnly: entry.Header.AceFlags&inheritOnlyACE != 0,
		})
	}
	return aces, nil
}

// describeTrustees prefers account names because a bare SID does not tell the user what to fix.
func describeTrustees(sids []string) []string {
	described := make([]string, 0, len(sids))
	for _, raw := range sids {
		described = append(described, describeTrustee(raw))
	}
	return described
}

func describeTrustee(raw string) string {
	sid, err := windows.StringToSid(raw)
	if err != nil {
		return raw
	}
	account, domain, _, err := sid.LookupAccount("")
	if err != nil || account == "" {
		return raw
	}
	if domain == "" {
		return account
	}
	return domain + `\` + account
}
