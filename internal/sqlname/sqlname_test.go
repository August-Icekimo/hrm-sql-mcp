package sqlname

import (
	"reflect"
	"testing"
)

func TestCandidates(t *testing.T) {
	cases := []struct {
		in   string
		want []Name
	}{
		{"sp_x", []Name{{Object: "sp_x"}}},
		{"[sp_x]", []Name{{Object: "sp_x"}}},
		{"dbo.sp_x", []Name{{Schema: "dbo", Object: "sp_x"}, {Object: "dbo.sp_x"}}},
		{"[dbo].[sp_x]", []Name{{Schema: "dbo", Object: "sp_x"}}},
		{" dbo . sp_x ", []Name{{Schema: "dbo", Object: "sp_x"}, {Object: "dbo . sp_x"}}},

		// EIP's shape: one bracketed identifier containing dots.
		{"[EIP.humanly.BasicSetup.PersonalRestDayBase_1]", []Name{{Object: "EIP.humanly.BasicSetup.PersonalRestDayBase_1"}}},
		{"[dbo].[EIP.humanly.BasicSetup.PersonalRestDayBase_1]", []Name{{Schema: "dbo", Object: "EIP.humanly.BasicSetup.PersonalRestDayBase_1"}}},
		{"dbo.[EIP.Dashboard_1]", []Name{{Schema: "dbo", Object: "EIP.Dashboard_1"}}},
		{`"EIP.Dashboard_1"`, []Name{{Object: "EIP.Dashboard_1"}}},

		// Undelimited and dotted: both readings for one dot, whole text beyond.
		{"EIP.Dashboard_1", []Name{{Schema: "EIP", Object: "Dashboard_1"}, {Object: "EIP.Dashboard_1"}}},
		{"EIP.humanly.BasicSetup.PersonalRestDayBase_1", []Name{{Object: "EIP.humanly.BasicSetup.PersonalRestDayBase_1"}}},

		// ]] is an escaped ] inside brackets.
		{"[a]]b]", []Name{{Object: "a]b"}}},
	}
	for _, c := range cases {
		got, err := Candidates(c.in)
		if err != nil {
			t.Errorf("Candidates(%q): %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Candidates(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestCandidatesRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"[unterminated",
		"dbo.",
		".sp_x",
		"[db].[dbo].[sp_x]", // cross-database: never resolved, so never guessed
	} {
		if got, err := Candidates(in); err == nil {
			t.Errorf("Candidates(%q) = %+v, want an error", in, got)
		}
	}
}

func TestQuotedRoundTrips(t *testing.T) {
	for _, n := range []Name{
		{Object: "sp_x"},
		{Schema: "dbo", Object: "EIP.humanly.X_1"},
		{Object: "a]b"},
	} {
		got, err := Candidates(n.Quoted())
		if err != nil {
			t.Fatalf("Candidates(%q): %v", n.Quoted(), err)
		}
		if len(got) != 1 || got[0] != n {
			t.Errorf("Candidates(%q) = %+v, want [%+v]", n.Quoted(), got, n)
		}
	}
}

func TestKeys(t *testing.T) {
	cases := map[string][]string{
		"[dbo].[sp_X]":      {"sp_x"},
		"dbo.sp_X":          {"sp_x", "dbo.sp_x"},
		"[EIP.humanly.X_1]": {"eip.humanly.x_1"},
		"EIP.humanly.X_1":   {"eip.humanly.x_1"},
		"EIP.Dashboard_1":   {"dashboard_1", "eip.dashboard_1"},
		"[unterminated":     {"[unterminated"},
	}
	for in, want := range cases {
		if got := Keys(in); !reflect.DeepEqual(got, want) {
			t.Errorf("Keys(%q) = %v, want %v", in, got, want)
		}
	}
}
