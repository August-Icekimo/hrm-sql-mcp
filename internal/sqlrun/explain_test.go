package sqlrun

import "testing"

// planXML is a trimmed but structurally faithful showplan, carrying the parts
// a live plan from the development container does not happen to produce:
// warnings and missing-index suggestions. Those need an index worth suggesting
// and a conversion that blocks a seek, neither of which exists on a heap in a
// fixture database — so without a fixture the parsing for them would ship
// untested.
const planXML = `<?xml version="1.0"?>
<ShowPlanXML xmlns="http://schemas.microsoft.com/sqlserver/2004/07/showplan" Version="1.599">
 <BatchSequence><Batch><Statements>
  <StmtSimple StatementText="SELECT emp_no FROM EMP_DATA WHERE emp_name = N'x'"
              StatementType="SELECT" StatementEstRows="12.5" StatementSubTreeCost="0.35612">
   <QueryPlan>
    <MissingIndexes>
     <MissingIndexGroup Impact="94.7">
      <MissingIndex Database="[hrm]" Schema="[dbo]" Table="[EMP_DATA]">
       <ColumnGroup Usage="EQUALITY"><Column Name="[emp_name]" ColumnId="3"/></ColumnGroup>
       <ColumnGroup Usage="INEQUALITY"><Column Name="[start_date]" ColumnId="7"/></ColumnGroup>
       <ColumnGroup Usage="INCLUDE">
        <Column Name="[emp_no]" ColumnId="1"/><Column Name="[salary]" ColumnId="9"/>
       </ColumnGroup>
      </MissingIndex>
     </MissingIndexGroup>
    </MissingIndexes>
    <Warnings>
     <PlanAffectingConvert ConvertIssue="Seek Plan" Expression="CONVERT_IMPLICIT(nvarchar(10),[emp_no])"/>
     <ColumnsWithNoStatistics><ColumnReference Table="[EMP_DATA]" Column="emp_name"/></ColumnsWithNoStatistics>
    </Warnings>
    <RelOp NodeId="0" PhysicalOp="Sort" LogicalOp="Sort" EstimateRows="12.5" EstimatedTotalSubtreeCost="0.35612">
     <Sort>
      <RelOp NodeId="1" PhysicalOp="Table Scan" LogicalOp="Table Scan" EstimateRows="15559" EstimatedTotalSubtreeCost="0.34190">
       <TableScan/>
      </RelOp>
     </Sort>
    </RelOp>
   </QueryPlan>
  </StmtSimple>
 </Statements></Batch></BatchSequence>
</ShowPlanXML>`

func TestParsePlan(t *testing.T) {
	p := parsePlan(planXML)

	if len(p.Statements) != 1 {
		t.Fatalf("got %d statements, want 1", len(p.Statements))
	}
	st := p.Statements[0]
	if st.EstimatedRows != 12.5 || st.EstimatedCost != 0.35612 || st.Type != "SELECT" {
		t.Errorf("statement = %+v", st)
	}

	// Nested RelOps must be found at any depth: the expensive operator is
	// usually the inner one, and a parser that only saw the top level would
	// report the plan as having no scans at all.
	if len(p.Operators) != 2 {
		t.Fatalf("got %d operators, want 2 (the nested one was missed)", len(p.Operators))
	}
	if p.Operators[0].EstimatedCost < p.Operators[1].EstimatedCost {
		t.Error("operators are not sorted by cost")
	}

	scans := p.Scans()
	if len(scans) != 1 || scans[0].Physical != "Table Scan" {
		t.Errorf("scans = %+v, want the one Table Scan", scans)
	}
	if scans[0].EstimatedRows != 15559 {
		t.Errorf("scan estimated rows = %v", scans[0].EstimatedRows)
	}
}

func TestParsePlanMissingIndex(t *testing.T) {
	p := parsePlan(planXML)
	if len(p.MissingIndexes) != 1 {
		t.Fatalf("got %d missing indexes, want 1", len(p.MissingIndexes))
	}
	mi := p.MissingIndexes[0]
	if mi.Table != "EMP_DATA" {
		t.Errorf("table = %q, want EMP_DATA with brackets stripped", mi.Table)
	}
	if mi.Impact != 94.7 {
		t.Errorf("impact = %v", mi.Impact)
	}
	// The three column groups must not be merged: which columns go in the key
	// and which are merely included is the whole content of the suggestion.
	if len(mi.Equality) != 1 || mi.Equality[0] != "emp_name" {
		t.Errorf("equality = %v", mi.Equality)
	}
	if len(mi.Inequality) != 1 || mi.Inequality[0] != "start_date" {
		t.Errorf("inequality = %v", mi.Inequality)
	}
	if len(mi.Include) != 2 {
		t.Errorf("include = %v, want two columns", mi.Include)
	}
}

func TestParsePlanWarnings(t *testing.T) {
	p := parsePlan(planXML)
	if len(p.Warnings) != 2 {
		t.Fatalf("got %d warnings, want 2: %+v", len(p.Warnings), p.Warnings)
	}

	byKind := map[string]PlanWarning{}
	for _, w := range p.Warnings {
		byKind[w.Kind] = w
	}
	conv, ok := byKind["PlanAffectingConvert"]
	if !ok {
		t.Fatalf("implicit conversion warning missing: %+v", p.Warnings)
	}
	// The detail is the actionable part — knowing a conversion happened is
	// useless without knowing which column it happened to.
	if conv.Detail == "" || !contains(conv.Detail, "emp_no") {
		t.Errorf("conversion detail = %q, should name the expression", conv.Detail)
	}
	if _, ok := byKind["ColumnsWithNoStatistics"]; !ok {
		t.Errorf("statistics warning missing: %+v", p.Warnings)
	}
}

// TestParsePlanIgnoresUnknownElements is the property the token walk buys:
// a plan from a newer server carrying elements this code has never heard of
// must parse, not fail.
func TestParsePlanIgnoresUnknownElements(t *testing.T) {
	weird := `<ShowPlanXML xmlns="http://schemas.microsoft.com/sqlserver/2004/07/showplan">
	 <SomeFutureThing Attr="1"><Nested/></SomeFutureThing>
	 <BatchSequence><Batch><Statements>
	  <StmtSimple StatementText="SELECT 1" StatementEstRows="1" StatementSubTreeCost="0.001">
	   <QueryPlan><Whatever/><RelOp PhysicalOp="Constant Scan" EstimateRows="1" EstimatedTotalSubtreeCost="0.001"/></QueryPlan>
	  </StmtSimple>
	 </Statements></Batch></BatchSequence></ShowPlanXML>`

	p := parsePlan(weird)
	if len(p.Statements) != 1 || len(p.Operators) != 1 {
		t.Fatalf("unknown elements broke parsing: %+v", p)
	}
	// A Constant Scan reads nothing; counting it as a scan would put a warning
	// on every trivial query and teach the reader to ignore the field.
	if len(p.Scans()) != 0 {
		t.Errorf("Constant Scan was reported as a scan")
	}
}

func TestPlanTotalCost(t *testing.T) {
	p := parsePlan(planXML)
	if got := p.TotalCost(); got != 0.35612 {
		t.Errorf("TotalCost() = %v", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
