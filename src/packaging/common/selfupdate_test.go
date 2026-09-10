package common

import "testing"

func TestSelfUpdateHopPersistsAndClears(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSelfUpdateHop(dir, "0.1.0-42.abcdef"); err != nil {
		t.Fatal(err)
	}
	if got := ReadSelfUpdateHop(dir); got != "0.1.0-42.abcdef" {
		t.Fatalf("hop = %q", got)
	}
	if err := ClearSelfUpdateHop(dir); err != nil {
		t.Fatal(err)
	}
	if got := ReadSelfUpdateHop(dir); got != "" {
		t.Fatalf("cleared hop = %q", got)
	}
}
