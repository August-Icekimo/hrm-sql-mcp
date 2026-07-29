package envcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, name, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestProcessEnvironmentWins is the precedence a CI runner depends on: a value
// exported for one command must beat a file the machine happens to carry.
func TestProcessEnvironmentWins(t *testing.T) {
	f := write(t, "settings.env", "HRM_SQL_TARGET_X_DATABASE=from_file\n", 0o644)
	t.Setenv("HRM_SQL_TARGET_X_DATABASE", "from_env")

	s, err := Load([]string{f}, true)
	if err != nil {
		t.Fatal(err)
	}
	v, o, ok := s.Lookup("HRM_SQL_TARGET_X_DATABASE")
	if !ok || v != "from_env" {
		t.Fatalf("got %q (ok=%v), want from_env", v, ok)
	}
	if o.Kind != "env" {
		t.Errorf("origin = %q, want env", o.Kind)
	}
}

func TestFileUsedWhenEnvironmentIsSilent(t *testing.T) {
	f := write(t, "settings.env", "HRM_SQL_TARGET_X_DATABASE=from_file\n", 0o644)

	s, err := Load([]string{f}, true)
	if err != nil {
		t.Fatal(err)
	}
	v, o, ok := s.Lookup("HRM_SQL_TARGET_X_DATABASE")
	if !ok || v != "from_file" {
		t.Fatalf("got %q (ok=%v), want from_file", v, ok)
	}
	if o.Kind != "file" || !strings.HasSuffix(o.Detail, "settings.env") {
		t.Errorf("origin = %+v, want the file path", o)
	}
}

// TestEarlierFileWins pins the documented order for several files.
func TestEarlierFileWins(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.env")
	b := filepath.Join(dir, "b.env")
	os.WriteFile(a, []byte("K=a\n"), 0o644)
	os.WriteFile(b, []byte("K=b\n"), 0o644)

	s, err := Load([]string{a, b}, true)
	if err != nil {
		t.Fatal(err)
	}
	if v, _, _ := s.Lookup("K"); v != "a" {
		t.Errorf("K = %q, want a (the earlier file)", v)
	}
}

// TestSecretFileMustBe0600 is the rule that keeps this from being a downgrade
// of the credentials handling it replaced.
func TestSecretFileMustBe0600(t *testing.T) {
	f := write(t, "creds.env", "HRM_SQL_LOCAL_RO_PASSWORD=hunter2\n", 0o644)

	_, err := Load([]string{f}, true)
	if err == nil {
		t.Fatal("a world-readable file holding a password was accepted")
	}
	for _, want := range []string{"0600", "chmod"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// TestSecretRuleFollowsContentNotFilename: keying it off the path would be
// defeated by naming the file something else.
func TestSecretRuleFollowsContentNotFilename(t *testing.T) {
	t.Run("no secrets, loose mode is fine", func(t *testing.T) {
		f := write(t, "anything.env", "HRM_SQL_TARGET_X_DATABASE=hrm\n", 0o644)
		if _, err := Load([]string{f}, true); err != nil {
			t.Errorf("a file of plain settings was refused: %v", err)
		}
	})
	t.Run("secret in an innocuously named file is still refused", func(t *testing.T) {
		f := write(t, "notes.txt", "HRM_SQL_LOCAL_RW_PASSWORD=x\n", 0o644)
		if _, err := Load([]string{f}, true); err == nil {
			t.Error("a password in a loosely named file was accepted")
		}
	})
	t.Run("secret at 0600 is accepted", func(t *testing.T) {
		f := write(t, "creds.env", "HRM_SQL_LOCAL_RO_PASSWORD=x\n", 0o600)
		if _, err := Load([]string{f}, true); err != nil {
			t.Errorf("a correctly protected credentials file was refused: %v", err)
		}
	})
}

// TestMissingFileIsSkippedWhenOptional is what lets the same command work on a
// laptop with a credentials file and in CI without one.
func TestMissingFileIsSkippedWhenOptional(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.env")

	if _, err := Load([]string{missing}, true); err != nil {
		t.Errorf("optional missing file was treated as an error: %v", err)
	}
	if _, err := Load([]string{missing}, false); err == nil {
		t.Error("a required missing file was accepted")
	}
}

func TestParsing(t *testing.T) {
	body := strings.Join([]string{
		"# a comment",
		"",
		"PLAIN=value",
		`QUOTED="quoted value"`,
		"SPACED  =  padded  ",
		"export EXPORTED=exported",
	}, "\n")
	f := write(t, "s.env", body, 0o644)

	s, err := Load([]string{f}, true)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"PLAIN":    "value",
		"QUOTED":   "quoted value",
		"SPACED":   "padded",
		"EXPORTED": "exported",
	} {
		if v, _, _ := s.Lookup(k); v != want {
			t.Errorf("%s = %q, want %q", k, v, want)
		}
	}
}

func TestIsSecretKey(t *testing.T) {
	for _, k := range []string{"HRM_SQL_X_RO_PASSWORD", "x_password", " A_PASSWORD "} {
		if !IsSecretKey(k) {
			t.Errorf("%q should be treated as a secret", k)
		}
	}
	for _, k := range []string{"HRM_SQL_X_RO_USER", "HRM_SQL_TARGET_X_HOST", "PASSWORD_HINT"} {
		if IsSecretKey(k) {
			t.Errorf("%q should not be treated as a secret", k)
		}
	}
}
