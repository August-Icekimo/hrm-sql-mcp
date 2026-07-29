package approver

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "approvals"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.Poll = 10 * time.Millisecond
	return s
}

func req(id, statement string) Request {
	return Request{ID: id, Tool: "mssql_execute", Alias: "hrm_0209", Statement: statement}
}

func TestApproveFlow(t *testing.T) {
	s := newStore(t)
	const stmt = "UPDATE EMP_DATA SET remark = 'x' WHERE emp_no = '1'"

	if err := s.Raise(req("abc", stmt)); err != nil {
		t.Fatalf("Raise: %v", err)
	}
	pending, err := s.Pending()
	if err != nil || len(pending) != 1 || pending[0].ID != "abc" {
		t.Fatalf("Pending() = %v, %v", pending, err)
	}
	if pending[0].StatementSHA256 != Hash(stmt) {
		t.Error("request does not carry the statement digest")
	}

	if err := s.Decide("abc", DecisionApprove, "tester", "looks fine"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	d, err := s.Wait(context.Background(), "abc", stmt)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if d.Decision != DecisionApprove || d.By != "tester" {
		t.Errorf("decision = %+v", d)
	}

	// Decided requests drop out of the pending list.
	if pending, _ := s.Pending(); len(pending) != 0 {
		t.Errorf("Pending() still returns %d after a decision", len(pending))
	}
}

func TestDenyIsAnError(t *testing.T) {
	s := newStore(t)
	const stmt = "DELETE FROM EMP_DATA"
	if err := s.Raise(req("deny1", stmt)); err != nil {
		t.Fatal(err)
	}
	if err := s.Decide("deny1", DecisionDeny, "tester", "no"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Wait(context.Background(), "deny1", stmt); !errors.Is(err, ErrDenied) {
		t.Errorf("err = %v, want ErrDenied", err)
	}
}

// TestApprovalIsBoundToItsStatement is the property that stops an approval
// being a bearer token: approve something harmless, then run something else
// under the same id.
func TestApprovalIsBoundToItsStatement(t *testing.T) {
	s := newStore(t)
	const approved = "UPDATE EMP_DATA SET remark = 'x' WHERE emp_no = '1'"
	const swapped = "DELETE FROM EMP_DATA"

	if err := s.Raise(req("swap", approved)); err != nil {
		t.Fatal(err)
	}
	if err := s.Decide("swap", DecisionApprove, "tester", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Wait(context.Background(), "swap", swapped); !errors.Is(err, ErrStatement) {
		t.Fatalf("err = %v, want ErrStatement — a decision must not authorise a different statement", err)
	}
	// The original statement still works, so the check is binding rather than
	// simply broken.
	if _, err := s.Wait(context.Background(), "swap", approved); err != nil {
		t.Errorf("the approved statement was rejected too: %v", err)
	}
}

func TestExpiry(t *testing.T) {
	s := newStore(t)
	s.TTL = 20 * time.Millisecond
	const stmt = "UPDATE T SET x = 1"
	if err := s.Raise(req("old", stmt)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)

	// Nothing may decide it any more...
	if err := s.Decide("old", DecisionApprove, "tester", ""); !errors.Is(err, ErrExpired) {
		t.Errorf("Decide on an expired request = %v, want ErrExpired", err)
	}
	// ...and waiting on it ends rather than hanging.
	if _, err := s.Wait(context.Background(), "old", stmt); !errors.Is(err, ErrExpired) {
		t.Errorf("Wait on an expired request = %v, want ErrExpired", err)
	}
	if pending, _ := s.Pending(); len(pending) != 0 {
		t.Errorf("an expired request is still listed as pending")
	}
}

func TestWaitRespectsContext(t *testing.T) {
	s := newStore(t)
	const stmt = "UPDATE T SET x = 1"
	if err := s.Raise(req("slow", stmt)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := s.Wait(ctx, "slow", stmt)
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v, want ErrTimeout", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("Wait did not honour the context deadline")
	}
}

func TestWaitOnUnknownRequest(t *testing.T) {
	s := newStore(t)
	if _, err := s.Wait(context.Background(), "nope", "SELECT 1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFilesArePrivate(t *testing.T) {
	s := newStore(t)
	if err := s.Raise(req("perm", "UPDATE T SET x = 1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Decide("perm", DecisionApprove, "tester", ""); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.requestPath("perm"), s.decisionPath("perm")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s has mode %04o; it holds a statement and its authorisation", p, perm)
		}
	}
	fi, err := os.Stat(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("approval directory has mode %04o", perm)
	}
}

const (
	childEnvDir = "HRM_APPROVER_TEST_DIR"
	childEnvID  = "HRM_APPROVER_TEST_ID"
)

// TestApprovalCrossesProcesses is the reason this is files and not memory.
// The waiting server and the approving terminal are different processes; an
// in-memory store would have the approval land somewhere the waiter cannot see.
func TestApprovalCrossesProcesses(t *testing.T) {
	if dir := os.Getenv(childEnvDir); dir != "" {
		// Child: play the part of `hrm-sql-mcp approve <id>`.
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("child Open: %v", err)
		}
		if err := s.Decide(os.Getenv(childEnvID), DecisionApprove, "child", "from another process"); err != nil {
			t.Fatalf("child Decide: %v", err)
		}
		return
	}

	s := newStore(t)
	const stmt = "UPDATE EMP_DATA SET remark = 'x' WHERE emp_no = '1'"
	if err := s.Raise(req("xproc", stmt)); err != nil {
		t.Fatal(err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		cmd := exec.Command(exe, "-test.run=TestApprovalCrossesProcesses")
		cmd.Env = append(os.Environ(), childEnvDir+"="+s.Dir(), childEnvID+"=xproc")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("child failed: %v\n%s", err, out)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := s.Wait(ctx, "xproc", stmt)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if d.By != "child" {
		t.Errorf("decision came from %q, want the child process", d.By)
	}
}

// TestConcurrentRaises checks that many servers raising at once do not corrupt
// each other's files — the same multi-process shape as the audit log.
func TestConcurrentRaises(t *testing.T) {
	s := newStore(t)
	done := make(chan struct{})
	const n = 20
	for i := range n {
		go func() {
			defer func() { done <- struct{}{} }()
			id := "req" + strconv.Itoa(i)
			if err := s.Raise(req(id, "UPDATE T SET x = "+strconv.Itoa(i))); err != nil {
				t.Errorf("Raise: %v", err)
			}
		}()
	}
	for range n {
		<-done
	}
	pending, err := s.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != n {
		t.Errorf("Pending() = %d, want %d", len(pending), n)
	}
	// No temporary files left behind by the atomic-write dance.
	entries, _ := os.ReadDir(s.Dir())
	for _, e := range entries {
		if len(e.Name()) > 0 && e.Name()[0] == '.' {
			t.Errorf("leftover temporary file %s", e.Name())
		}
	}
}
