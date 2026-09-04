package main

import "testing"

func TestRemovalMarkerRequiresExactTerminalValue(t *testing.T) {
	if err := validateRemovalMarker([]byte(removalMarkerContents)); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{nil, []byte("terminal-removal"), []byte("update\n"), []byte(removalMarkerContents + "extra")} {
		if err := validateRemovalMarker(invalid); err == nil {
			t.Fatalf("invalid marker %q was accepted", invalid)
		}
	}
}
