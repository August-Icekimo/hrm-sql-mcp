package service

import (
	"context"
	"fmt"
	"sync"

	"github.com/codex-k8s/hrm-sql-mcp/internal/audit"
	"github.com/codex-k8s/hrm-sql-mcp/internal/schemadict"
	"github.com/codex-k8s/hrm-sql-mcp/internal/spdb"
	"github.com/codex-k8s/hrm-sql-mcp/internal/sqlrun"
	"github.com/codex-k8s/hrm-sql-mcp/internal/target"
)

// Explain returns the estimated plan for a statement without running it.
func (s *Service) Explain(ctx context.Context, alias, statement string, includeXML bool) (*sqlrun.Plan, *target.Target, error) {
	const tool = "mssql_explain"
	db, t, err := s.open(ctx, alias, tool)
	if err != nil {
		return nil, nil, err
	}

	plan, perr := sqlrun.Explain(ctx, db, statement, s.queryLimits(), includeXML)

	// Audited like any other statement. It does not execute, but it is still
	// an agent putting SQL in front of this database, and a log that recorded
	// only the statements that ran would answer "what was tried" with silence.
	ev := audit.Event{Tool: tool, Statement: statement}
	if plan != nil {
		ev.Rows = len(plan.Operators)
	}
	if err := s.finish(ev, t, s.login(ctx, db, t.Alias()), perr); err != nil {
		return nil, t, err
	}
	return plan, t, nil
}

// Deps returns what an object references, or what references it.
func (s *Service) Deps(ctx context.Context, alias, name string, dir spdb.Direction) ([]spdb.Dependency, *target.Target, error) {
	const tool = "mssql_deps"
	db, t, err := s.open(ctx, alias, tool)
	if err != nil {
		return nil, nil, err
	}
	cctx, cancel := s.catalogContext(ctx)
	defer cancel()

	deps, derr := spdb.Deps(cctx, db, name, dir)
	ev := audit.Event{Tool: tool, Statement: string(dir) + " " + name, Rows: len(deps)}
	if err := s.finish(ev, t, s.login(ctx, db, t.Alias()), derr); err != nil {
		return nil, t, err
	}
	return deps, t, nil
}

// dictOnce loads the data dictionary at most once per process.
type dictCache struct {
	once sync.Once
	dict *schemadict.Dict
	err  error
}

// SchemaSearch searches the project's data dictionary.
//
// No database connection is involved, and so no target: the dictionary is a
// file in the repository. That also means it can be stale, which the caller is
// told rather than left to assume — the file's path is returned so a doubtful
// result can be checked against the live catalog with mssql_query.
func (s *Service) SchemaSearch(ctx context.Context, query string, opt schemadict.SearchOptions) ([]schemadict.Match, string, error) {
	const tool = "mssql_schema_search"

	path := s.ProjectPath(s.pol.Paths.SchemaDict)
	if path == "" {
		err := fmt.Errorf("no schema dictionary is configured (policy: paths.schema_dict)")
		s.deny(tool, "", err)
		return nil, "", err
	}

	s.dict.once.Do(func() {
		s.dict.dict, s.dict.err = schemadict.Load(path)
	})
	if s.dict.err != nil {
		err := fmt.Errorf("load schema dictionary %s: %w", path, s.dict.err)
		s.deny(tool, "", err)
		return nil, path, err
	}

	matches := s.dict.dict.Search(query, opt)
	if err := s.finish(audit.Event{Tool: tool, Statement: query, Rows: len(matches)}, nil, "", nil); err != nil {
		return nil, path, err
	}
	return matches, path, nil
}

// SchemaTable returns one table's dictionary entry.
func (s *Service) SchemaTable(ctx context.Context, name string) (schemadict.Table, string, error) {
	matches, path, err := s.SchemaSearch(ctx, name, schemadict.SearchOptions{Limit: 1, TablesOnly: true})
	if err != nil {
		return schemadict.Table{}, path, err
	}
	if len(matches) == 0 {
		return schemadict.Table{}, path, fmt.Errorf("no table named %q in the dictionary", name)
	}
	t, ok := s.dict.dict.TableByName(matches[0].Table)
	if !ok {
		return schemadict.Table{}, path, fmt.Errorf("no table named %q in the dictionary", name)
	}
	return t, path, nil
}
