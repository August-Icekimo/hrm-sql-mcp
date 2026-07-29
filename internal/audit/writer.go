package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MaxLine bounds one record including its newline.
//
// PIPE_BUF on Linux is 4096. That constant is about pipes, not regular files,
// but it is the size below which a single write is reliably not split, and
// this package's cross-process guarantee rests on records not being split.
// Choosing the conservative bound costs a clipped statement now and then;
// choosing a larger one would cost a corrupted log, discovered only when
// somebody needed to read it.
const MaxLine = 4096

// ErrRecordTooLarge is returned when a record cannot be brought under MaxLine
// even with every variable-length field removed.
//
// It is an error rather than a best-effort write. A line over the bound risks
// interleaving with another process's line, and two damaged records are worse
// than one refused operation: the caller finds out immediately, instead of
// somebody discovering unparseable JSON in six months.
var ErrRecordTooLarge = errors.New("audit record cannot be reduced below the line limit")

// Writer appends records to the log.
type Writer struct {
	path string
	f    *os.File
	// mu covers concurrent Write calls inside this process. Between processes
	// the guarantee comes from O_APPEND and MaxLine, not from this lock —
	// a lock cannot span processes, which is exactly the case that matters.
	mu sync.Mutex
}

// Open opens or creates the log.
//
// The file is 0600 and the directory 0700: these records carry the SQL
// statements an agent ran against HRM, which means employee numbers, salary
// figures, and enough of a schema map to be worth reading. An existing file
// with wider permissions is refused rather than quietly narrowed — somebody
// widened it on purpose, and silently undoing that would hide the decision.
//
// A leading ~/ is expanded, because that is how the path is written in the
// policy file.
func Open(path string) (*Writer, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("audit file path is empty; auditing has no off switch")
	}
	path = expandHome(path)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create audit directory: %w", err)
	}
	if info, err := os.Stat(path); err == nil {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return nil, fmt.Errorf(
				"audit log %s has mode %04o, must not be readable by others (run: chmod 600 %s)",
				path, perm, path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat audit log: %w", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log %s: %w", path, err)
	}
	return &Writer{path: path, f: f}, nil
}

// Path returns the log's location, for error messages that need to name it.
func (w *Writer) Path() string { return w.path }

// Write appends one record.
//
// Callers must treat a failure as fatal to the operation they were about to
// perform. An unaudited query against this database is the thing the audit
// exists to prevent, so "the log was full" is not a reason to proceed quietly.
func (w *Writer) Write(ev Event) error {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if ev.CorrelationID == "" {
		ev.CorrelationID = NewID()
	}
	if ev.PID == 0 {
		ev.PID = os.Getpid()
	}

	line, err := encode(ev)
	if err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.f.Write(line)
	if err != nil {
		return fmt.Errorf("write audit record: %w", err)
	}
	if n != len(line) {
		return fmt.Errorf("short audit write: %d of %d bytes", n, len(line))
	}
	return nil
}

// Close releases the file.
func (w *Writer) Close() error {
	if w == nil || w.f == nil {
		return nil
	}
	return w.f.Close()
}

// encode marshals a record and shrinks it until it fits MaxLine.
//
// The loop halves whichever variable-length field is currently longest and
// re-marshals, rather than computing a budget from the fixed fields. Budget
// arithmetic has to predict how much JSON escaping will expand a string —
// which depends on the Chinese text and quoting in these statements — and a
// prediction that is wrong by one byte produces exactly the oversized line the
// bound exists to prevent. Measuring is slower and cannot be wrong.
func encode(ev Event) ([]byte, error) {
	for {
		line, err := json.Marshal(ev)
		if err != nil {
			return nil, fmt.Errorf("encode audit record: %w", err)
		}
		if len(line)+1 <= MaxLine {
			return append(line, '\n'), nil
		}
		if !shrink(&ev) {
			return nil, fmt.Errorf("%w (%d bytes, limit %d)", ErrRecordTooLarge, len(line)+1, MaxLine)
		}
	}
}

// shrink halves the longest clippable field, reporting whether it could.
func shrink(ev *Event) bool {
	switch {
	case len(ev.Statement) >= len(ev.Error) && len(ev.Statement) > 0:
		ev.Statement = clip(ev.Statement)
		markClipped(ev, "statement")
		return true
	case len(ev.Error) > 0:
		ev.Error = clip(ev.Error)
		markClipped(ev, "error")
		return true
	default:
		return false
	}
}

// clip halves a string on a rune boundary, emptying it once it is tiny.
func clip(s string) string {
	r := []rune(s)
	half := len(r) / 2
	if half < 8 {
		return ""
	}
	return string(r[:half])
}

func markClipped(ev *Event, field string) {
	for _, f := range ev.Clipped {
		if f == field {
			return
		}
	}
	ev.Clipped = append(ev.Clipped, field)
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}
