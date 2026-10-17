package watch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFingerprint(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	os.WriteFile(f, []byte("one"), 0o644)
	a := Fingerprint([]string{dir})
	if a != Fingerprint([]string{dir}) {
		t.Fatal("fingerprint not stable")
	}
	os.WriteFile(f, []byte("one two"), 0o644)
	if Fingerprint([]string{dir}) == a {
		t.Fatal("fingerprint did not change after a write")
	}
	if Fingerprint([]string{filepath.Join(dir, "missing")}) != Fingerprint(nil) {
		t.Fatal("a missing dir should contribute nothing")
	}
}
