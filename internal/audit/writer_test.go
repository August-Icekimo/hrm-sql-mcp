package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestWriteAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	if err := w.Write(Event{Tool: "mssql_query", Alias: "local_hrm", Outcome: OutcomeOK, Rows: 3}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var got Event
	if err := json.Unmarshal([]byte(readLines(t, path)[0]), &got); err != nil {
		t.Fatalf("record is not valid JSON: %v", err)
	}
	if got.Tool != "mssql_query" || got.Rows != 3 {
		t.Errorf("round trip lost fields: %+v", got)
	}
	// The three fields a caller should never have to remember to set.
	if got.Time.IsZero() {
		t.Error("Time was not filled in")
	}
	if got.CorrelationID == "" {
		t.Error("CorrelationID was not filled in")
	}
	if got.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", got.PID, os.Getpid())
	}
}

// TestEveryRecordFitsTheBound is the invariant the cross-process guarantee
// rests on. A statement far over the limit must come back clipped and marked,
// never written oversized.
func TestEveryRecordFitsTheBound(t *testing.T) {
	tests := []struct {
		name string
		ev   Event
	}{
		{"huge statement", Event{Tool: "mssql_query", Statement: strings.Repeat("SELECT * FROM PAYROLL ", 2000)}},
		{"huge error", Event{Tool: "mssql_query", Error: strings.Repeat("deadlock ", 2000)}},
		{"both huge", Event{
			Tool:      "mssql_query",
			Statement: strings.Repeat("薪資查詢 ", 2000),
			Error:     strings.Repeat("錯誤訊息 ", 2000),
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line, err := encode(tc.ev)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if len(line) > MaxLine {
				t.Fatalf("line is %d bytes, over the %d limit", len(line), MaxLine)
			}
			var got Event
			if err := json.Unmarshal(line, &got); err != nil {
				t.Fatalf("clipped record is not valid JSON: %v", err)
			}
			if len(got.Clipped) == 0 {
				t.Error("record was shortened without saying so; a clipped statement " +
					"must not be indistinguishable from a short one")
			}
			// Clipping must not cut a multi-byte character in half.
			if strings.ContainsRune(got.Statement, '�') || strings.ContainsRune(got.Error, '�') {
				t.Error("clipping split a rune")
			}
		})
	}
}

// TestRecordTooLargeIsRefused covers the case where the fixed fields alone
// blow the bound: it must fail loudly rather than write a line that could
// interleave with another process's.
func TestRecordTooLargeIsRefused(t *testing.T) {
	_, err := encode(Event{Tool: strings.Repeat("t", MaxLine*2)})
	if err == nil {
		t.Fatal("oversized record was accepted")
	}
	if !strings.Contains(err.Error(), "line limit") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestOpenRefusesAWorldReadableLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("opened a log others can read")
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open after chmod: %v", err)
	}
	w.Close()
}

func TestOpenCreatesTheDirectoryPrivately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "hrm-sql-mcp", "audit.jsonl")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	fi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("audit directory mode %04o lets others in", perm)
	}
	fi, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("audit file mode %04o lets others in", perm)
	}
}

func TestOpenRefusesAnEmptyPath(t *testing.T) {
	if _, err := Open("  "); err == nil {
		t.Fatal("an empty path was accepted; auditing must not be silently disabled")
	}
}

func TestAppendsRatherThanTruncates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	for i := range 3 {
		w, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Write(Event{Tool: "t", Rows: i}); err != nil {
			t.Fatal(err)
		}
		w.Close()
	}
	if got := len(readLines(t, path)); got != 3 {
		t.Errorf("%d lines after three sessions, want 3 — reopening truncated the log", got)
	}
}

func TestConcurrentWritesWithinOneProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.Write(Event{Tool: "mssql_query", Rows: i}); err != nil {
				t.Errorf("Write: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := len(readLines(t, path)); got != 50 {
		t.Errorf("%d lines, want 50", got)
	}
}

// childEnv marks a re-executed copy of this test binary as a writer process.
const (
	childEnvPath  = "HRM_AUDIT_TEST_CHILD_PATH"
	childEnvIndex = "HRM_AUDIT_TEST_CHILD_INDEX"
	childRecords  = 200
	childCount    = 8
)

// TestConcurrentAppendAcrossProcesses is the test this package exists to pass.
//
// It re-executes the test binary so the writers are genuinely separate
// processes. Goroutines would prove nothing here: they share the Writer's
// mutex and the same file descriptor, so they exercise the one mechanism that
// is not in question. The claim under test — that Claude Code and Gemini CLI
// can append to one log without damaging it — is about processes that share
// neither.
func TestConcurrentAppendAcrossProcesses(t *testing.T) {
	if path := os.Getenv(childEnvPath); path != "" {
		runChild(t, path, os.Getenv(childEnvIndex))
		return
	}

	path := filepath.Join(t.TempDir(), "audit.jsonl")
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}

	var wg sync.WaitGroup
	for i := range childCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(exe, "-test.run=TestConcurrentAppendAcrossProcesses")
			cmd.Env = append(os.Environ(),
				childEnvPath+"="+path,
				childEnvIndex+"="+strconv.Itoa(i))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("child %d failed: %v\n%s", i, err, out)
			}
		}()
	}
	wg.Wait()

	lines := readLines(t, path)
	want := childCount * childRecords
	if len(lines) != want {
		t.Fatalf("%d lines, want %d — records were lost or split", len(lines), want)
	}

	// Every line parses, and every (child, sequence) pair arrives exactly
	// once. Line count alone would not catch two half-records that happen to
	// add up to the right number of newlines.
	seen := map[string]bool{}
	pids := map[int]bool{}
	transitions, prevPID := 0, -1
	for i, ln := range lines {
		var ev Event
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Fatalf("line %d is not valid JSON (interleaved write): %v\n%s", i+1, err, ln)
		}
		key := ev.Actor + "/" + strconv.Itoa(ev.Rows)
		if seen[key] {
			t.Errorf("duplicate record %s", key)
		}
		seen[key] = true
		pids[ev.PID] = true
		if ev.PID != prevPID {
			transitions++
			prevPID = ev.PID
		}
	}
	if len(seen) != want {
		t.Errorf("%d distinct records, want %d", len(seen), want)
	}
	if len(pids) != childCount {
		t.Errorf("records came from %d processes, want %d", len(pids), childCount)
	}

	// How much this run actually proved. If the children happened to be
	// scheduled one after another there would be exactly childCount runs of
	// consecutive PIDs, and the file was never written to concurrently — the
	// test would have passed without exercising anything. Reported rather than
	// asserted, because scheduling is not ours to guarantee and a flaky test
	// here would get deleted.
	t.Logf("%d lines from %d processes, %d PID transitions", len(lines), len(pids), transitions)
	if transitions <= childCount {
		t.Logf("WARNING: writes did not interleave, so concurrent append was not exercised")
	}
}

// runChild writes this process's share of the records.
func runChild(t *testing.T, path, index string) {
	w, err := Open(path)
	if err != nil {
		t.Fatalf("child Open: %v", err)
	}
	defer w.Close()
	for i := range childRecords {
		// A statement long enough to make records realistically sized, so the
		// test exercises the bound rather than tiny lines that would fit
		// under any implementation.
		ev := Event{
			Actor:     "child" + index,
			Tool:      "mssql_query",
			Alias:     "local_hrm",
			Server:    "127.0.0.1:1433",
			Database:  "hrm",
			Login:     "hrm_mcp_ro",
			Mode:      "readonly",
			Statement: fmt.Sprintf("SELECT %s FROM EMPLOYEE WHERE emp_no = '%04d'", strings.Repeat("col, ", 40), i),
			Outcome:   OutcomeOK,
			Rows:      i,
		}
		if err := w.Write(ev); err != nil {
			t.Fatalf("child Write: %v", err)
		}
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, MaxLine*2), MaxLine*2)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return out
}
