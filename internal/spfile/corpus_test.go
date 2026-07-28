package spfile

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestRealCorpus runs the decoder over HRM's actual stored-procedure
// directory. Synthetic fixtures cannot prove the encoding handling is right;
// this corpus is the reason the package exists.
//
// Skipped when the directory is absent so the suite still runs anywhere.
func TestRealCorpus(t *testing.T) {
	dir := os.Getenv("HRM_SP_DIR")
	if dir == "" {
		t.Skip("set HRM_SP_DIR to run against the real corpus")
	}
	scripts, failures, err := ScanDir(dir)
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}
	if len(failures) > 0 {
		for f, e := range failures {
			t.Errorf("failed to decode %s: %v", f, e)
		}
	}

	enc := map[string]int{}
	procs := map[string]struct{}{}
	noDef := 0
	for _, s := range scripts {
		enc[s.Encoding]++
		if !utf8.ValidString(s.Definition) {
			t.Errorf("%s: definition is not valid UTF-8", s.Path)
		}
		if strings.ContainsRune(s.Definition, utf8.RuneError) {
			t.Errorf("%s: definition contains a replacement character", s.Path)
		}
		for _, p := range s.Procs {
			procs[p] = struct{}{}
		}
		if s.Definition == "" {
			noDef++
		}
	}
	t.Logf("files=%d  encodings=%v  distinct procs=%d  files with no procedure=%d",
		len(scripts), enc, len(procs), noDef)
}
