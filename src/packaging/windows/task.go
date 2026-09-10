// Package windows owns the per-user Task Scheduler entry. The task runs in the interactive
// user's security context because the shipper reads that user's coding-agent stores.
//
// Task Scheduler terminates an action rather than delivering the Unix signals used by the
// shipper's drain path. The supervisor therefore puts the child in a kill-on-close Job Object:
// stopping the task is deterministic, but not graceful. This is safe because a source is only
// committed after its destination confirms the write, so an interrupted tick is replayed.
package windows

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

type Spec = common.Spec
type Status = common.Status

// legacyTaskName is the machine-global name used before names were per-user. Task Scheduler's
// namespace is machine-wide, so it let one user's install overwrite another's — and made a second
// user's registration fail outright when they could not write the first user's task.
const legacyTaskName = `\Quesma Shipper`
const taskRunnerName = "quesma-shipper-supervisor.exe"

// taskName suffixes the SID rather than the account name: a machine can see the same account name
// in more than one domain, and a name collision here is what the suffix exists to prevent.
func taskName(userSID string) string { return legacyTaskName + " - " + userSID }

func taskRunner(executable string) string {
	if slash := strings.LastIndexAny(executable, `\/`); slash >= 0 {
		return executable[:slash+1] + taskRunnerName
	}
	return taskRunnerName
}

func programFromTask(command string) string {
	slash := strings.LastIndexAny(command, `\/`)
	base := command
	if slash >= 0 {
		base = command[slash+1:]
	}
	if strings.EqualFold(base, taskRunnerName) {
		return command[:slash+1] + "quesma-shipper.exe"
	}
	return command
}

// renderTask points at the stable runner so every TUF replacement remains under Task Scheduler.
// The account name goes in the description because the name itself carries only the SID.
func renderTask(spec Spec, userSID, userName string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Collect local AI-agent trajectories, scrub and encrypt them, and send them to your organisation. Installed for %s.</Description>
    <URI>%s</URI>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <Delay>PT30S</Delay>
      <UserId>%s</UserId>
      <Repetition>
        <Interval>PT1H</Interval>
      </Repetition>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>%s</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure>
      <Interval>PT15M</Interval>
      <Count>3</Count>
    </RestartOnFailure>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
      <Arguments>"%s"</Arguments>
    </Exec>
  </Actions>
</Task>
`, xmlText(userName), xmlText(taskName(userSID)), xmlText(userSID), xmlText(userSID),
		xmlText(taskRunner(spec.Executable)), xmlText(spec.LogDir))
}

type taskDocument struct {
	Settings struct {
		Enabled *bool `xml:"Enabled"`
	} `xml:"Settings"`
	Actions struct {
		Exec struct {
			Command string `xml:"Command"`
		} `xml:"Exec"`
	} `xml:"Actions"`
	Principals struct {
		Principal struct {
			UserID string `xml:"UserId"`
		} `xml:"Principal"`
	} `xml:"Principals"`
}

// enabled treats an absent Settings/Enabled as enabled. Task Scheduler stores no element for a
// setting left at its default, so reading a missing one as false calls a healthy task disabled.
func (d taskDocument) enabled() bool {
	return d.Settings.Enabled == nil || *d.Settings.Enabled
}

// legacyTaskIsOurs reports whether the pre-rename task belongs to this user: another user's task is
// neither ours to retire nor, without their permissions, deletable.
func legacyTaskIsOurs(doc taskDocument, userSID string) bool {
	owner := doc.Principals.Principal.UserID
	return owner != "" && strings.EqualFold(owner, userSID)
}

func parseTask(raw []byte) (taskDocument, error) {
	raw = taskXMLUTF8(raw)
	var doc taskDocument
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return doc, err
	}
	return doc, nil
}

// taskXMLForSchtasks emits the Unicode file format expected by schtasks /Create /XML.
func taskXMLForSchtasks(raw string) []byte {
	raw = strings.Replace(raw, `encoding="UTF-8"`, `encoding="UTF-16"`, 1)
	units := utf16.Encode([]rune(raw))
	encoded := make([]byte, 2+2*len(units))
	encoded[0], encoded[1] = 0xff, 0xfe
	for i, unit := range units {
		encoded[2+i*2] = byte(unit)
		encoded[3+i*2] = byte(unit >> 8)
	}
	return encoded
}

// taskXMLUTF8 normalizes what schtasks actually writes when stdout is redirected: UTF-16 with a
// BOM on some Windows versions, and on others single-byte text that still declares UTF-16, which
// encoding/xml refuses outright. Only interleaved NUL bytes distinguish the two without a BOM.
func taskXMLUTF8(raw []byte) []byte {
	switch {
	case len(raw) >= 2 && raw[0] == 0xff && raw[1] == 0xfe:
		raw = decodeUTF16LE(raw[2:])
	case bytes.IndexByte(raw[:min(len(raw), 64)], 0) >= 0:
		raw = decodeUTF16LE(raw)
	}
	raw = bytes.Replace(raw, []byte(`encoding="UTF-16"`), []byte(`encoding="UTF-8"`), 1)
	return bytes.Replace(raw, []byte(`encoding="utf-16"`), []byte(`encoding="UTF-8"`), 1)
}

func decodeUTF16LE(raw []byte) []byte {
	units := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		units = append(units, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	return []byte(string(utf16.Decode(units)))
}

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
