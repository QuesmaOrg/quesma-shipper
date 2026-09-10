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

const TaskName = `\Quesma Shipper`
const taskRunnerName = "quesma-shipper-supervisor.exe"

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
func renderTask(spec Spec, userSID string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Collect local AI-agent trajectories, scrub and encrypt them, and send them to your organisation.</Description>
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
`, TaskName, xmlText(userSID), xmlText(userSID), xmlText(taskRunner(spec.Executable)), xmlText(spec.LogDir))
}

type taskDocument struct {
	Settings struct {
		Enabled bool `xml:"Enabled"`
	} `xml:"Settings"`
	Actions struct {
		Exec struct {
			Command string `xml:"Command"`
		} `xml:"Exec"`
	} `xml:"Actions"`
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

// schtasks emits UTF-16 XML on some Windows versions even when stdout is redirected.
func taskXMLUTF8(raw []byte) []byte {
	if len(raw) < 2 || raw[0] != 0xff || raw[1] != 0xfe {
		return raw
	}
	units := make([]uint16, 0, (len(raw)-2)/2)
	for i := 2; i+1 < len(raw); i += 2 {
		units = append(units, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	decoded := []byte(string(utf16.Decode(units)))
	decoded = bytes.Replace(decoded, []byte(`encoding="UTF-16"`), []byte(`encoding="UTF-8"`), 1)
	decoded = bytes.Replace(decoded, []byte(`encoding="utf-16"`), []byte(`encoding="UTF-8"`), 1)
	return decoded
}

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
