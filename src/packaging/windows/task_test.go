package windows

import (
	"encoding/binary"
	"encoding/xml"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

func TestTaskRunsAsTheInteractiveUserAndSurvivesUpdates(t *testing.T) {
	spec := Spec{
		Executable: `C:\Users\A & B\Quesma Shipper\quesma-shipper.exe`,
		LogDir:     `C:\Users\A & B\AppData\Local\quesma-shipper\logs`,
	}
	raw := renderTask(spec, "S-1-5-21-123")

	for _, want := range []string{
		`<LogonType>InteractiveToken</LogonType>`,
		`<RunLevel>LeastPrivilege</RunLevel>`,
		`<Interval>PT1H</Interval>`,
		`<RestartOnFailure>`,
		`C:\Users\A &amp; B\Quesma Shipper\quesma-shipper-supervisor.exe`,
		`<Arguments>"C:\Users\A &amp; B\AppData\Local\quesma-shipper\logs"</Arguments>`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("task XML lacks %q:\n%s", want, raw)
		}
	}
	doc, err := parseTask([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Settings.Enabled || doc.Actions.Exec.Command != taskRunner(spec.Executable) {
		t.Fatalf("parsed task = %+v", doc)
	}
}

func TestParseTaskAcceptsSchtasksUTF16Output(t *testing.T) {
	raw := `<?xml version="1.0" encoding="UTF-16"?><Task xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"><Settings><Enabled>true</Enabled></Settings><Actions><Exec><Command>C:\shipper.exe</Command></Exec></Actions></Task>`
	units := utf16.Encode([]rune(raw))
	encoded := make([]byte, 2+2*len(units))
	encoded[0], encoded[1] = 0xff, 0xfe
	for i, unit := range units {
		binary.LittleEndian.PutUint16(encoded[2+i*2:], unit)
	}
	doc, err := parseTask(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Settings.Enabled || doc.Actions.Exec.Command != `C:\shipper.exe` {
		t.Fatalf("parsed task = %+v", doc)
	}
}

func TestTaskXMLForSchtasksIsUTF16AndRoundTrips(t *testing.T) {
	raw := renderTask(common.Spec{Executable: `C:\Quesma Shipper\quesma-shipper.exe`}, "S-1-5-21-1")
	encoded := taskXMLForSchtasks(raw)
	if len(encoded) < 2 || encoded[0] != 0xff || encoded[1] != 0xfe {
		t.Fatalf("task XML lacks UTF-16LE BOM: %x", encoded[:min(len(encoded), 8)])
	}
	doc, err := parseTask(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Actions.Exec.Command != `C:\Quesma Shipper\quesma-shipper-supervisor.exe` {
		t.Fatalf("parsed command = %q", doc.Actions.Exec.Command)
	}
}

func TestTaskXMLIsWellFormed(t *testing.T) {
	if err := xml.Unmarshal([]byte(renderTask(common.Spec{Executable: `C:\shipper.exe`}, "S-1-5-21-1")), new(any)); err != nil {
		t.Fatal(err)
	}
}

func TestTaskRunnerMapsBackToTheOwnedProgram(t *testing.T) {
	runner := `C:\Users\Jane\Quesma Shipper\QUESMA-SHIPPER-SUPERVISOR.EXE`
	want := `C:\Users\Jane\Quesma Shipper\quesma-shipper.exe`
	if got := programFromTask(runner); got != want {
		t.Fatalf("programFromTask(%q) = %q, want %q", runner, got, want)
	}
}
