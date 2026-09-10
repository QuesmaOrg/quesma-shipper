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
	raw := renderTask(spec, "S-1-5-21-123", `WINDOWSSHIPPER\a & b`)

	for _, want := range []string{
		`<LogonType>InteractiveToken</LogonType>`,
		`<RunLevel>LeastPrivilege</RunLevel>`,
		`<URI>\Quesma Shipper - S-1-5-21-123</URI>`,
		`Installed for WINDOWSSHIPPER\a &amp; b.`,
		// Task Scheduler rejects a zero Duration; omitting it is what repeats indefinitely.
		"<Repetition>\n        <Interval>PT1H</Interval>\n      </Repetition>",
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
	if !doc.enabled() || doc.Actions.Exec.Command != taskRunner(spec.Executable) {
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
	if !doc.enabled() || doc.Actions.Exec.Command != `C:\shipper.exe` {
		t.Fatalf("parsed task = %+v", doc)
	}
}

// Windows 11 26200 writes single-byte text that still declares UTF-16, with no BOM and a doubled
// CR. Read literally that is neither valid UTF-16 nor an encoding encoding/xml will accept.
func TestParseTaskAcceptsSingleByteOutputThatDeclaresUTF16(t *testing.T) {
	raw := "<?xml version=\"1.0\" encoding=\"UTF-16\"?>\r\r\n" +
		`<Task xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">` +
		`<Settings><Enabled>true</Enabled></Settings>` +
		`<Principals><Principal id="Author"><UserId>S-1-5-21-7-1001</UserId></Principal></Principals>` +
		`<Actions><Exec><Command>C:\shipper.exe</Command></Exec></Actions></Task>`
	doc, err := parseTask([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !doc.enabled() || doc.Actions.Exec.Command != `C:\shipper.exe` {
		t.Fatalf("parsed task = %+v", doc)
	}
	if !legacyTaskIsOurs(doc, "S-1-5-21-7-1001") {
		t.Error("the principal did not survive the encoding fixup, so a legacy task cannot be retired")
	}
}

// Task Scheduler stores no element for a setting left at its default, so a live enabled task comes
// back with no Settings/Enabled at all. Reading that as false reports a running task as disabled.
func TestParseTaskTreatsAnAbsentEnabledAsEnabled(t *testing.T) {
	raw := `<Task xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">` +
		`<Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>` +
		`<StartWhenAvailable>true</StartWhenAvailable></Settings>` +
		`<Actions><Exec><Command>C:\shipper.exe</Command></Exec></Actions></Task>`
	doc, err := parseTask([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !doc.enabled() {
		t.Error("a task with no Settings/Enabled was reported as disabled")
	}

	disabled := `<Task xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">` +
		`<Settings><Enabled>false</Enabled></Settings></Task>`
	doc, err = parseTask([]byte(disabled))
	if err != nil {
		t.Fatal(err)
	}
	if doc.enabled() {
		t.Error("an explicitly disabled task was reported as enabled")
	}
}

func TestTaskXMLForSchtasksIsUTF16AndRoundTrips(t *testing.T) {
	raw := renderTask(common.Spec{Executable: `C:\Quesma Shipper\quesma-shipper.exe`}, "S-1-5-21-1", "jane")
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
	if err := xml.Unmarshal([]byte(renderTask(common.Spec{Executable: `C:\shipper.exe`}, "S-1-5-21-1", "jane")), new(any)); err != nil {
		t.Fatal(err)
	}
}

// Task Scheduler's namespace is machine-wide, so two users' installs must not name the same task.
func TestTaskNameIsPerUser(t *testing.T) {
	first, second := taskName("S-1-5-21-7-1001"), taskName("S-1-5-21-7-1002")
	if first == second {
		t.Fatalf("both users got the task name %q", first)
	}
	if first == legacyTaskName || second == legacyTaskName {
		t.Fatalf("per-user name collides with the pre-rename name %q", legacyTaskName)
	}
	for _, name := range []string{first, second} {
		// schtasks resolves a name without a leading separator against the root folder anyway, but
		// the XML URI has to match what /Create was given.
		if !strings.HasPrefix(name, `\`) {
			t.Errorf("task name %q is not rooted", name)
		}
		if strings.ContainsAny(strings.TrimPrefix(name, `\`), `\/:*?"<>|`) {
			t.Errorf("task name %q contains a character Task Scheduler forbids", name)
		}
	}
}

func TestLegacyTaskIsOursOnlyForThisUsersOwnTask(t *testing.T) {
	ours := renderTask(common.Spec{Executable: `C:\shipper.exe`}, "S-1-5-21-7-1001", "jane")
	doc, err := parseTask([]byte(ours))
	if err != nil {
		t.Fatal(err)
	}
	if !legacyTaskIsOurs(doc, "s-1-5-21-7-1001") {
		t.Error("a task whose principal is this user, in a different case, was not recognized")
	}
	if legacyTaskIsOurs(doc, "S-1-5-21-7-1002") {
		t.Error("another user's task was treated as ours to retire")
	}
	if legacyTaskIsOurs(taskDocument{}, "S-1-5-21-7-1001") {
		t.Error("a task with no principal was treated as ours to retire")
	}
}

func TestTaskRunnerMapsBackToTheOwnedProgram(t *testing.T) {
	runner := `C:\Users\Jane\Quesma Shipper\QUESMA-SHIPPER-SUPERVISOR.EXE`
	want := `C:\Users\Jane\Quesma Shipper\quesma-shipper.exe`
	if got := programFromTask(runner); got != want {
		t.Fatalf("programFromTask(%q) = %q, want %q", runner, got, want)
	}
}
