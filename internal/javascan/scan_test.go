package javascan

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLiterals(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []Literal
	}{
		{
			name: "plain literal",
			src:  `String s = "{call sp_ema0100(?)}";`,
			want: []Literal{{Text: `{call sp_ema0100(?)}`, Line: 1}},
		},
		{
			name: "escaped quote does not end the literal",
			src:  `String s = "a \" sp_inside b"; // sp_comment`,
			want: []Literal{{Text: `a \" sp_inside b`, Line: 1}},
		},
		{
			name: "trailing backslash before the closing quote",
			src:  `String p = "C:\\temp\\"; String q = "sp_after";`,
			want: []Literal{{Text: `C:\\temp\\`, Line: 1}, {Text: `sp_after`, Line: 1}},
		},
		{
			name: "line comment is skipped",
			src:  "// call sp_ghost\nString s = \"sp_real\";",
			want: []Literal{{Text: `sp_real`, Line: 2}},
		},
		{
			name: "block comment is skipped and lines still count",
			src:  "/* sp_ghost\n   sp_ghost2 */\nString s = \"sp_real\";",
			want: []Literal{{Text: `sp_real`, Line: 3}},
		},
		{
			name: "a quote inside a char literal does not open a string",
			src:  `char c = '"'; String s = "sp_real";`,
			want: []Literal{{Text: `sp_real`, Line: 1}},
		},
		{
			name: "escaped char literal",
			src:  `char c = '\''; String s = "sp_real";`,
			want: []Literal{{Text: `sp_real`, Line: 1}},
		},
		{
			name: "a comment marker inside a literal is not a comment",
			src:  `String s = "-- sp_real /* still text";`,
			want: []Literal{{Text: `-- sp_real /* still text`, Line: 1}},
		},
		{
			name: "text block",
			src:  "String s = \"\"\"\n  exec sp_real\n  \"\"\";",
			want: []Literal{{Text: "\n  exec sp_real\n  ", Line: 1}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Literals(tc.src)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Literals()\n got: %#v\nwant: %#v", got, tc.want)
			}
		})
	}
}

// TestScanIgnoresCommentsAndCode is the point of the package: the same
// identifier in a comment, in a variable name, and in a literal must not all
// count as call sites.
func TestScanIgnoresCommentsAndCode(t *testing.T) {
	dir := t.TempDir()
	src := `package a;
// TODO: retire sp_in_line_comment
/* sp_in_block_comment */
public class A {
    private String sp_variable_name = null;
    void go() {
        String q = "{call sp_called(?,?)}";
        String r = "exec dbo.sp_also_called";
        // String dead = "sp_in_commented_out_code";
    }
}
`
	if err := os.WriteFile(filepath.Join(dir, "A.java"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-Java file must not be read at all.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte(`"sp_not_java"`), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Scan(dir, Options{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Files != 1 {
		t.Errorf("Files = %d, want 1", res.Files)
	}
	if len(res.Failures) != 0 {
		t.Errorf("Failures = %v, want none", res.Failures)
	}

	want := map[string]bool{"sp_called": true, "sp_also_called": true}
	for name := range res.ByName {
		if !want[name] {
			t.Errorf("found %q, which is not a call site", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("missed call site %q", name)
	}

	if sites := res.ByName["sp_called"]; len(sites) != 1 || sites[0].Line != 7 {
		t.Errorf("sp_called sites = %#v, want one at line 7", sites)
	}
	if sites := res.ByName["sp_called"]; len(sites) == 1 {
		if sites[0].File != "A.java" {
			t.Errorf("File = %q, want the path relative to the scanned root", sites[0].File)
		}
		if sites[0].Kind != KindCall {
			t.Errorf("Kind = %q, want %q for a {call ...} escape", sites[0].Kind, KindCall)
		}
	}
}

// TestCallVersusMention is the distinction that keeps the ghost list credible.
// The alias case is taken verbatim from HRM's WDC0900Action, where a plain
// name search reported sp_amount as a called procedure.
func TestCallVersusMention(t *testing.T) {
	dir := t.TempDir()
	src := "class A {\n" +
		"  String alias = \"select isnull(sum(x),0) sp_amount from T\";\n" +
		"  String jdbc  = \"{call sp_ema0100(?,?)}\";\n" +
		"  String exec1 = \"exec sp_wdb1500 ?,?\";\n" +
		"  String exec2 = \"EXECUTE [dbo].[sp_bracketed]\";\n" +
		"  String name  = \"sp_built_at_runtime\";\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(dir, "A.java"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Scan(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}

	wantKind := map[string]Kind{
		"sp_amount":           KindMention,
		"sp_ema0100":          KindCall,
		"sp_wdb1500":          KindCall,
		"sp_bracketed":        KindCall,
		"sp_built_at_runtime": KindMention,
	}
	for name, want := range wantKind {
		sites := res.ByName[name]
		if len(sites) != 1 {
			t.Errorf("%s: got %d sites, want 1", name, len(sites))
			continue
		}
		if sites[0].Kind != want {
			t.Errorf("%s: Kind = %q, want %q", name, sites[0].Kind, want)
		}
	}
	if got := len(res.Calls("sp_amount")); got != 0 {
		t.Errorf("a column alias produced %d call sites; it must produce none", got)
	}
	if got := len(res.Calls("sp_ema0100")); got != 1 {
		t.Errorf("Calls(sp_ema0100) = %d, want 1", got)
	}
}

func TestPathPrefix(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "A.java"),
		[]byte(`class A { String q = "exec sp_x"; }`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Scan(dir, Options{PathPrefix: "src"})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Sites[0].File; got != "src/A.java" {
		t.Errorf("File = %q, want %q", got, "src/A.java")
	}
}

// TestScanDedupesWithinOneLiteral guards the call-site count from being
// inflated by a literal that names the same procedure twice.
func TestScanDedupesWithinOneLiteral(t *testing.T) {
	dir := t.TempDir()
	src := `class A { String q = "if 1=1 exec sp_x else exec sp_x"; }`
	if err := os.WriteFile(filepath.Join(dir, "A.java"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Scan(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(res.ByName["sp_x"]); got != 1 {
		t.Errorf("sp_x call sites = %d, want 1", got)
	}
}

// TestScanSkipsBuildOutput keeps generated sources out of the inventory; a
// copy of a Java file under build/ would double every call-site count.
func TestScanSkipsBuildOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "build", "gen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build", "gen", "B.java"),
		[]byte(`class B { String q = "sp_generated"; }`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Scan(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 0 {
		t.Errorf("Files = %d, want 0 (build/ should be pruned)", res.Files)
	}
}
