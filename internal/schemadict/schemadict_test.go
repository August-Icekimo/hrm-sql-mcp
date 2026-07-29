package schemadict

import (
	"os"
	"path/filepath"
	"testing"
)

// sample mirrors the real file's shape, including the parts of it that are
// wrong: "nan" placeholders and a 欄位說明 section claiming every column is a
// primary key.
const sample = `# 資料表：檔案代號

**所屬系統**：子作業

**說明**：

## 欄位定義

| 序號 | 欄位名稱 | 中文說明 | 資料型別 | 允許空值 | 主鍵 | 備註 |
|------|----------|----------|----------|----------|------|------|
| 序 | 欄位代號 | 欄位名稱 | 型態長度 |  | PK | 備註 |

---

# 資料表：ADVANCE_BONUS_GRANT

**所屬系統**：借支獎金

**說明**：獎金發放主檔

## 欄位定義

| 序號 | 欄位名稱 | 中文說明 | 資料型別 | 允許空值 | 主鍵 | 備註 |
|------|----------|----------|----------|----------|------|------|
| 1 | data_yy | 年度 | varchar(4) |  | V | Y |
| 2 | emp_no | 員工編號 | varchar(10) |  | nan | nan |
| 3 | leave_d_days | 事假日時數 | decimal |  | nan | nan |

## 欄位說明

- **data_yy** （主鍵）: 年度，型別為 varchar(4)，不可為空，Y
- **emp_no** （主鍵）: 員工編號，型別為 varchar(10)，不可為空，nan
- **leave_d_days** （主鍵）: 事假日時數，型別為 decimal，不可為空，nan

---

# 資料表：EMP_DATA

**所屬系統**：員工管理

## 欄位定義

| 序號 | 欄位名稱 | 中文說明 | 資料型別 | 允許空值 | 主鍵 | 備註 |
|------|----------|----------|----------|----------|------|------|
| 1 | emp_no | 員工編號 | varchar(10) |  | V | nan |
| 2 | emp_name | 中文姓名 | nvarchar(20) | Y | nan | nan |
`

func TestParse(t *testing.T) {
	d := Parse(sample)
	if len(d.Tables) != 3 {
		t.Fatalf("got %d tables, want 3", len(d.Tables))
	}

	tbl, ok := d.TableByName("ADVANCE_BONUS_GRANT")
	if !ok {
		t.Fatal("ADVANCE_BONUS_GRANT not parsed")
	}
	if tbl.System != "借支獎金" || tbl.Note != "獎金發放主檔" {
		t.Errorf("system=%q note=%q", tbl.System, tbl.Note)
	}
	// Three data rows. The header row and the |---| separator must not become
	// columns, or every table gains two phantom fields.
	if len(tbl.Columns) != 3 {
		t.Fatalf("got %d columns, want 3: %+v", len(tbl.Columns), tbl.Columns)
	}

	c := tbl.Columns[2]
	if c.Name != "leave_d_days" || c.Description != "事假日時數" || c.Type != "decimal" {
		t.Errorf("column = %+v", c)
	}
	// "nan" is the generator's placeholder, not a value.
	if c.Remark != "" {
		t.Errorf("remark = %q, want empty (nan must be normalised away)", c.Remark)
	}
	if c.KeyMarked {
		t.Errorf("leave_d_days should not be key-marked")
	}
	if !tbl.Columns[0].KeyMarked {
		t.Errorf("data_yy has 主鍵=V and should be key-marked")
	}
}

// TestParseIgnoresTheProseSection is the point of reading only 欄位定義.
// The 欄位說明 section marks every column （主鍵）, so a parser that read it
// would report all three columns as primary keys.
func TestParseIgnoresTheProseSection(t *testing.T) {
	d := Parse(sample)
	tbl, _ := d.TableByName("ADVANCE_BONUS_GRANT")
	marked := 0
	for _, c := range tbl.Columns {
		if c.KeyMarked {
			marked++
		}
	}
	if marked != 1 {
		t.Errorf("%d columns key-marked, want 1 — the 欄位說明 prose was read", marked)
	}
	// And it must not have invented extra columns out of the prose bullets.
	if len(tbl.Columns) != 3 {
		t.Errorf("got %d columns; prose bullets leaked in", len(tbl.Columns))
	}
}

func TestSearch(t *testing.T) {
	d := Parse(sample)

	tests := []struct {
		name      string
		query     string
		wantTable string
		wantCol   string
		wantField MatchField
	}{
		{"Chinese description", "事假", "ADVANCE_BONUS_GRANT", "leave_d_days", MatchDescription},
		{"column identifier", "leave_d_days", "ADVANCE_BONUS_GRANT", "leave_d_days", MatchColumnName},
		{"table name", "EMP_DATA", "EMP_DATA", "", MatchTableName},
		{"business area", "員工管理", "EMP_DATA", "", MatchSystem},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := d.Search(tc.query, SearchOptions{})
			if len(got) == 0 {
				t.Fatalf("no match for %q", tc.query)
			}
			top := got[0]
			if top.Table != tc.wantTable || top.Field != tc.wantField {
				t.Errorf("top match = table %q field %q, want %q/%q",
					top.Table, top.Field, tc.wantTable, tc.wantField)
			}
			if tc.wantCol != "" && (top.Column == nil || top.Column.Name != tc.wantCol) {
				t.Errorf("top match column = %v, want %q", top.Column, tc.wantCol)
			}
		})
	}
}

// TestSearchRanksExactFirst: emp_no appears in two tables and as a substring
// of nothing else here, but emp_name also contains "emp". An exact identifier
// must not be buried under partial hits.
func TestSearchRanksExactFirst(t *testing.T) {
	d := Parse(sample)
	got := d.Search("emp_no", SearchOptions{})
	if len(got) == 0 {
		t.Fatal("no matches")
	}
	if got[0].Column == nil || got[0].Column.Name != "emp_no" {
		t.Errorf("top match = %+v, want the exact emp_no column", got[0])
	}
}

func TestSearchTablesOnly(t *testing.T) {
	d := Parse(sample)
	got := d.Search("emp", SearchOptions{TablesOnly: true})
	for _, m := range got {
		if m.Column != nil {
			t.Errorf("tables-only search returned a column: %+v", m.Column)
		}
	}
	if len(got) == 0 {
		t.Error("tables-only search found nothing for \"emp\"")
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	d := Parse(sample)
	if got := d.Search("   ", SearchOptions{}); got != nil {
		t.Errorf("blank query returned %d matches; it should match nothing", len(got))
	}
}

func TestSearchLimit(t *testing.T) {
	d := Parse(sample)
	if got := d.Search("e", SearchOptions{Limit: 2}); len(got) > 2 {
		t.Errorf("limit ignored: %d matches", len(got))
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.md")
	if err := os.WriteFile(path, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(d.Tables) != 3 || d.Path != path {
		t.Errorf("loaded %d tables from %q", len(d.Tables), d.Path)
	}
}

// TestRealDictionary runs against HRM's actual file when it is available.
// The synthetic sample cannot prove the parser survives 10,821 lines of
// machine-generated Markdown; this is the reason the package exists.
func TestRealDictionary(t *testing.T) {
	path := os.Getenv("HRM_SCHEMA_DICT")
	if path == "" {
		t.Skip("set HRM_SCHEMA_DICT to run against the real dictionary")
	}
	d, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cols := 0
	for _, tbl := range d.Tables {
		cols += len(tbl.Columns)
		if tbl.Name == "" {
			t.Error("a table was parsed with no name")
		}
		for _, c := range tbl.Columns {
			if c.Name == "" {
				t.Errorf("%s has a column with no name", tbl.Name)
			}
			if c.Remark == "nan" || c.Type == "nan" || c.Description == "nan" {
				t.Errorf("%s.%s still carries a nan placeholder", tbl.Name, c.Name)
			}
		}
	}
	t.Logf("tables=%d columns=%d", len(d.Tables), cols)
	if len(d.Tables) < 100 {
		t.Errorf("only %d tables parsed; the real file has over 200", len(d.Tables))
	}
}
