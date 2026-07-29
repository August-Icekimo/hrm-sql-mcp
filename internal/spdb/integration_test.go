package spdb_test

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/codex-k8s/hrm-sql-mcp/internal/spdb"
	"github.com/codex-k8s/hrm-sql-mcp/internal/testenv"
)

func TestListAndLoadAllAgree(t *testing.T) {
	db, _ := testenv.Open(t)
	ctx := context.Background()

	list, err := spdb.List(ctx, db)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	full, err := spdb.LoadAll(ctx, db)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("no procedures found; the fixture database is not loaded")
	}
	if len(list) != len(full) {
		t.Errorf("List returned %d procedures and LoadAll %d; they must see the same catalog",
			len(list), len(full))
	}
	t.Logf("%d procedures", len(full))

	// List must not carry definitions, or callers would pay for text they did
	// not ask for and could not tell was missing.
	for _, p := range list {
		if p.Definition != "" {
			t.Fatalf("List returned a definition for %s", p.Qualified())
		}
	}

	for _, p := range full {
		if p.Encrypted {
			continue
		}
		if p.Definition == "" {
			t.Errorf("%s has no definition and is not marked encrypted", p.Qualified())
			continue
		}
		// nvarchar over a driver that mishandled UTF-16 would show up here,
		// and every diff downstream would be wrong.
		if !utf8.ValidString(p.Definition) {
			t.Errorf("%s: definition is not valid UTF-8", p.Qualified())
		}
		if !strings.Contains(strings.ToLower(p.Definition), p.Key()) {
			t.Errorf("%s: definition does not mention its own name", p.Qualified())
		}
	}
}

func TestGet(t *testing.T) {
	db, _ := testenv.Open(t)
	ctx := context.Background()

	all, err := spdb.LoadAll(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var sample spdb.Proc
	for _, p := range all {
		if !p.Encrypted {
			sample = p
			break
		}
	}
	if sample.Name == "" {
		t.Skip("no readable procedure to sample")
	}

	// Bare name, qualified name and bracketed name must all reach the same row.
	for _, name := range []string{sample.Name, sample.Qualified(), "[" + sample.Schema + "].[" + sample.Name + "]"} {
		got, err := spdb.Get(ctx, db, name)
		if err != nil {
			t.Errorf("Get(%q): %v", name, err)
			continue
		}
		if got.Definition != sample.Definition {
			t.Errorf("Get(%q) returned a different definition than LoadAll", name)
		}
	}

	if _, err := spdb.Get(ctx, db, "sp_this_does_not_exist_0000"); err == nil {
		t.Error("Get on a missing procedure returned no error")
	}
}

func TestIndexIsCaseInsensitive(t *testing.T) {
	db, _ := testenv.Open(t)
	all, err := spdb.LoadAll(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	byKey, dups := spdb.Index(all)
	if len(dups) > 0 {
		t.Logf("names present in more than one schema: %v", dups)
	}
	for _, p := range all {
		if _, ok := byKey[strings.ToLower(p.Name)]; !ok {
			t.Errorf("%s is missing from the index", p.Qualified())
		}
	}
}
