package approver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Decisions.
const (
	DecisionApprove = "approve"
	DecisionDeny    = "deny"
)

// Request is a pending approval, written by the process that wants to write.
type Request struct {
	ID string `json:"id"`
	// Created is when it was raised; Expires is when it stops being usable.
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`

	Actor string `json:"actor,omitempty"`
	Tool  string `json:"tool"`

	Alias    string `json:"alias"`
	Server   string `json:"server"`
	Database string `json:"database"`
	Login    string `json:"login"`

	// Statement is the exact text that will run if this is approved.
	Statement string `json:"statement"`
	// StatementSHA256 binds the decision to that text. Without it an approval
	// is a bearer token: approve a harmless statement, then run anything.
	StatementSHA256 string `json:"statement_sha256"`

	// Summary and Objects come from the classifier, to tell the approver what
	// they are looking at. Advisory: see package tsql.
	Summary string   `json:"summary,omitempty"`
	Objects []string `json:"objects,omitempty"`
	// Rehearsal carries what a commit:false run reported, when there was one.
	Rehearsal string `json:"rehearsal,omitempty"`
}

// Decision is a person's answer.
type Decision struct {
	ID       string    `json:"id"`
	Decision string    `json:"decision"`
	By       string    `json:"by,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	At       time.Time `json:"at"`
	// StatementSHA256 is copied from the request at approval time and checked
	// again before the statement runs.
	StatementSHA256 string `json:"statement_sha256"`
}

// Errors callers distinguish.
var (
	ErrNotFound  = errors.New("no such approval request")
	ErrExpired   = errors.New("approval request expired")
	ErrDenied    = errors.New("approval denied")
	ErrTimeout   = errors.New("timed out waiting for approval")
	ErrStatement = errors.New("approved statement does not match the statement about to run")
)

// Store is a directory holding requests and decisions.
type Store struct {
	dir string
	// TTL bounds how long a request stays usable.
	TTL time.Duration
	// Poll is how often Wait re-reads the directory.
	Poll time.Duration
}

// Open prepares the approval directory.
//
// 0700, like the audit log: a request file contains the statement about to run
// against a payroll database, and a decision file is what authorises it. Both
// are worth as much as the credentials.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("approval directory is empty")
	}
	dir = expandHome(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create approval directory: %w", err)
	}
	return &Store{dir: dir, TTL: 15 * time.Minute, Poll: 500 * time.Millisecond}, nil
}

// Dir returns the directory, for messages that tell a person where to look.
func (s *Store) Dir() string { return s.dir }

// Hash is the statement digest used to bind a decision to its statement.
func Hash(statement string) string {
	sum := sha256.Sum256([]byte(statement))
	return hex.EncodeToString(sum[:])
}

func (s *Store) requestPath(id string) string  { return filepath.Join(s.dir, id+".request.json") }
func (s *Store) decisionPath(id string) string { return filepath.Join(s.dir, id+".decision.json") }

// Raise records a pending request.
func (s *Store) Raise(r Request) error {
	if r.ID == "" {
		return errors.New("request needs an id")
	}
	now := time.Now()
	r.Created = now
	r.Expires = now.Add(s.TTL)
	r.StatementSHA256 = Hash(r.Statement)
	return writeJSON(s.requestPath(r.ID), r)
}

// Get reads one request.
func (s *Store) Get(id string) (Request, error) {
	var r Request
	raw, err := os.ReadFile(s.requestPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return r, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("parse approval request %s: %w", id, err)
	}
	return r, nil
}

// Pending lists requests that have not been decided and have not expired.
func (s *Store) Pending() ([]Request, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Request
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".request.json") {
			continue
		}
		id := strings.TrimSuffix(name, ".request.json")
		r, err := s.Get(id)
		if err != nil {
			continue
		}
		if time.Now().After(r.Expires) {
			continue
		}
		if _, err := os.Stat(s.decisionPath(id)); err == nil {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// Decide records an answer.
//
// It refuses to decide an expired request rather than letting a stale one
// through: the statement was reviewed against a database state that may no
// longer hold.
func (s *Store) Decide(id, decision, by, reason string) error {
	if decision != DecisionApprove && decision != DecisionDeny {
		return fmt.Errorf("decision must be %q or %q", DecisionApprove, DecisionDeny)
	}
	r, err := s.Get(id)
	if err != nil {
		return err
	}
	if time.Now().After(r.Expires) {
		return fmt.Errorf("%w: %s (raised %s)", ErrExpired, id, r.Created.Format(time.RFC3339))
	}
	return writeJSON(s.decisionPath(id), Decision{
		ID:              id,
		Decision:        decision,
		By:              by,
		Reason:          reason,
		At:              time.Now(),
		StatementSHA256: r.StatementSHA256,
	})
}

// Wait blocks until the request is decided, expires, or ctx ends.
//
// Polling rather than inotify: the decision may be written by a different
// process on a path that could be a network mount, and a missed notification
// here means a write that hangs forever. Re-reading a small directory twice a
// second is cheap enough that the simpler mechanism wins.
func (s *Store) Wait(ctx context.Context, id, statement string) (Decision, error) {
	want := Hash(statement)
	ticker := time.NewTicker(s.Poll)
	defer ticker.Stop()

	for {
		d, err := s.decision(id)
		switch {
		case err == nil:
			if d.Decision == DecisionDeny {
				return d, fmt.Errorf("%w: %s", ErrDenied, d.Reason)
			}
			// The approval is for one statement. Checking again here is what
			// stops an approval being spent on something else — including by
			// this process, if a retry rebuilt the statement differently.
			if d.StatementSHA256 != want {
				return d, ErrStatement
			}
			return d, nil
		case !errors.Is(err, ErrNotFound):
			return d, err
		}

		r, err := s.Get(id)
		if err != nil {
			return Decision{}, err
		}
		if time.Now().After(r.Expires) {
			return Decision{}, fmt.Errorf("%w: %s", ErrExpired, id)
		}

		select {
		case <-ctx.Done():
			return Decision{}, fmt.Errorf("%w: %w", ErrTimeout, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *Store) decision(id string) (Decision, error) {
	var d Decision
	raw, err := os.ReadFile(s.decisionPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		// A half-written decision file: the writer is mid-rename, or a crash
		// left a fragment. Treated as "not decided yet" so the wait continues
		// rather than failing on a transient state.
		return d, ErrNotFound
	}
	return d, nil
}

// writeJSON writes atomically: a reader polling this directory must never see
// half a file, and rename within a directory is atomic on POSIX.
func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}
