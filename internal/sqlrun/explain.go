package sqlrun

import (
	"context"
	"database/sql"
	"encoding/xml"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Plan is a parsed estimated execution plan.
type Plan struct {
	// Statements is one entry per statement in the batch.
	Statements []PlanStatement `json:"statements,omitempty"`
	// Operators are the plan's relational operators, most expensive first.
	Operators []PlanOperator `json:"operators,omitempty"`
	// MissingIndexes are the server's own index suggestions.
	MissingIndexes []MissingIndex `json:"missing_indexes,omitempty"`
	// Warnings are the plan's warnings: implicit conversions, missing
	// statistics, tempdb spills.
	Warnings []PlanWarning `json:"warnings,omitempty"`
	// XML is the raw plan, included only when asked for.
	XML string `json:"xml,omitempty"`
	// XMLBytes is its size even when the XML itself is omitted.
	XMLBytes int `json:"xml_bytes"`
}

// PlanStatement is one statement's headline numbers.
type PlanStatement struct {
	Text          string  `json:"text"`
	Type          string  `json:"type,omitempty"`
	EstimatedRows float64 `json:"estimated_rows"`
	EstimatedCost float64 `json:"estimated_cost"`
}

// PlanOperator is one relational operator.
type PlanOperator struct {
	Physical      string  `json:"physical"`
	Logical       string  `json:"logical,omitempty"`
	EstimatedRows float64 `json:"estimated_rows"`
	EstimatedCost float64 `json:"estimated_cost"`
}

// IsScan reports operators that read more than they are asked for. These are
// what turns a fast query slow as a table grows, and the reason to look at a
// plan at all.
func (o PlanOperator) IsScan() bool {
	return strings.Contains(o.Physical, "Scan") && !strings.Contains(o.Physical, "Constant")
}

// MissingIndex is a suggestion the optimiser attached to the plan.
type MissingIndex struct {
	Table      string   `json:"table"`
	Impact     float64  `json:"impact"`
	Equality   []string `json:"equality,omitempty"`
	Inequality []string `json:"inequality,omitempty"`
	Include    []string `json:"include,omitempty"`
}

// PlanWarning is one warning element from the plan.
type PlanWarning struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
}

// Explain returns the estimated execution plan for a statement.
//
// The statement is compiled, not run. SET SHOWPLAN_XML ON makes the server
// return a plan for everything that follows instead of executing it, which is
// what lets this tool answer "what would this do" for a query nobody wants to
// actually run yet.
//
// That property is worth stating precisely, because it is easy to over-trust:
// the statement is not executed, but it is still compiled, so it must be valid
// and the login must be able to see the objects it names. It is not a way to
// inspect a query the guard or the permissions would otherwise refuse.
func Explain(ctx context.Context, db *sql.DB, statement string, lim Limits, includeXML bool) (*Plan, error) {
	if strings.TrimSpace(statement) == "" {
		return nil, ErrEmptyStatement
	}
	lim = lim.withDefaults()

	// A dedicated connection for the same reason Query needs one: SHOWPLAN is
	// session state. Setting it on a borrowed connection and running the
	// statement on another would execute the statement for real.
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Close()

	qctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()

	if _, err := conn.ExecContext(qctx, "SET SHOWPLAN_XML ON"); err != nil {
		return nil, wrapErr(qctx, ctx, fmt.Errorf("enable showplan (does this login have GRANT SHOWPLAN?): %w", err))
	}
	// Turned off explicitly rather than relying on the connection being
	// discarded. If it were ever returned to the pool still in SHOWPLAN mode,
	// the next caller's query would silently return a plan instead of data.
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(qctx), "SET SHOWPLAN_XML OFF")
	}()

	rows, err := conn.QueryContext(qctx, statement)
	if err != nil {
		return nil, wrapErr(qctx, ctx, err)
	}
	defer rows.Close()

	var planXML strings.Builder
	for rows.Next() {
		var chunk string
		if err := rows.Scan(&chunk); err != nil {
			return nil, fmt.Errorf("read plan: %w", err)
		}
		planXML.WriteString(chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapErr(qctx, ctx, err)
	}
	if planXML.Len() == 0 {
		return nil, fmt.Errorf("the server returned no plan for this statement")
	}

	plan := parsePlan(planXML.String())
	if includeXML {
		plan.XML = planXML.String()
	}
	return plan, nil
}

// parsePlan pulls the useful facts out of the showplan XML.
//
// A token walk, not a set of structs mirroring the schema. Showplan XML is
// large, deeply recursive (RelOp contains operators that contain RelOps to
// arbitrary depth) and versioned by server build. Modelling it would mean a
// lot of types that break on the next SQL Server release, to extract perhaps
// fifteen attributes. Walking tokens and picking up the elements we recognise
// ignores everything else by construction, including elements that did not
// exist when this was written.
func parsePlan(planXML string) *Plan {
	p := &Plan{XMLBytes: len(planXML)}
	dec := xml.NewDecoder(strings.NewReader(planXML))

	var mi *MissingIndex
	var colUsage string
	// Warnings nest: <Warnings><ColumnsWithNoStatistics><ColumnReference/></…>.
	// Only the direct children of <Warnings> are warnings; the level below
	// carries their detail. Tracking depth keeps ColumnReference from being
	// reported as a warning in its own right, which would pad the list with
	// entries that say nothing and make the real ones easy to skim past.
	depth, warnDepth := 0, -1
	var pending *PlanWarning

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch t.Name.Local {
			case "StmtSimple":
				p.Statements = append(p.Statements, PlanStatement{
					Text:          attr(t, "StatementText"),
					Type:          attr(t, "StatementType"),
					EstimatedRows: attrFloat(t, "StatementEstRows"),
					EstimatedCost: attrFloat(t, "StatementSubTreeCost"),
				})
			case "RelOp":
				p.Operators = append(p.Operators, PlanOperator{
					Physical:      attr(t, "PhysicalOp"),
					Logical:       attr(t, "LogicalOp"),
					EstimatedRows: attrFloat(t, "EstimateRows"),
					EstimatedCost: attrFloat(t, "EstimatedTotalSubtreeCost"),
				})
			case "MissingIndexGroup":
				mi = &MissingIndex{Impact: attrFloat(t, "Impact")}
			case "MissingIndex":
				if mi != nil {
					mi.Table = strings.Trim(attr(t, "Table"), "[]")
				}
			case "ColumnGroup":
				colUsage = attr(t, "Usage")
			case "Column":
				if mi == nil {
					break
				}
				name := strings.Trim(attr(t, "Name"), "[]")
				switch colUsage {
				case "EQUALITY":
					mi.Equality = append(mi.Equality, name)
				case "INEQUALITY":
					mi.Inequality = append(mi.Inequality, name)
				case "INCLUDE":
					mi.Include = append(mi.Include, name)
				}
			case "Warnings":
				warnDepth = depth
			default:
				switch {
				case warnDepth > 0 && depth == warnDepth+1:
					// A direct child of <Warnings>: this is a warning, and its
					// element name is the kind. Matching on names we recognise
					// instead would miss the kinds a future server adds, which
					// are exactly the ones worth seeing.
					pending = &PlanWarning{Kind: t.Name.Local, Detail: warningDetail(t)}
				case pending != nil:
					// Deeper: detail belonging to the warning above it, such as
					// the ColumnReference naming the column with no statistics.
					// Knowing statistics are missing is useless without it.
					if d := warningDetail(t); d != "" {
						pending.Detail = strings.TrimSpace(pending.Detail + " " + d)
					}
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "MissingIndexGroup":
				if mi != nil {
					p.MissingIndexes = append(p.MissingIndexes, *mi)
					mi = nil
				}
			case "ColumnGroup":
				colUsage = ""
			case "Warnings":
				warnDepth = -1
			default:
				if pending != nil && depth == warnDepth+1 {
					p.Warnings = append(p.Warnings, *pending)
					pending = nil
				}
			}
			depth--
		}
	}

	sort.SliceStable(p.Operators, func(i, j int) bool {
		return p.Operators[i].EstimatedCost > p.Operators[j].EstimatedCost
	})
	return p
}

// warningDetail joins the attributes that carry the warning's substance.
func warningDetail(t xml.StartElement) string {
	var parts []string
	for _, a := range t.Attr {
		switch a.Name.Local {
		case "ConvertIssue", "Expression", "Statistics", "Table", "Column", "Reason", "SpillLevel":
			parts = append(parts, a.Name.Local+"="+a.Value)
		}
	}
	return strings.Join(parts, " ")
}

func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func attrFloat(t xml.StartElement, name string) float64 {
	v, err := strconv.ParseFloat(attr(t, name), 64)
	if err != nil {
		return 0
	}
	return v
}

// Scans returns the scan operators, most expensive first.
func (p *Plan) Scans() []PlanOperator {
	var out []PlanOperator
	for _, o := range p.Operators {
		if o.IsScan() {
			out = append(out, o)
		}
	}
	return out
}

// TotalCost is the whole batch's estimated cost.
func (p *Plan) TotalCost() float64 {
	var sum float64
	for _, s := range p.Statements {
		sum += s.EstimatedCost
	}
	return sum
}
