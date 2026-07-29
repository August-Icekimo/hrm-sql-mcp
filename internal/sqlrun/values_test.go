package sqlrun

import (
	"testing"
	"time"
)

func TestConvert(t *testing.T) {
	tests := []struct {
		name     string
		in       any
		typeName string
		want     any
	}{
		{"null", nil, "INT", nil},
		{"int passes through", int64(42), "INT", int64(42)},
		{"nvarchar passes through", "薪資", "NVARCHAR", "薪資"},

		// The three families the driver all hands back as []byte.
		{"decimal keeps its exact text", []byte("12345.6789"), "DECIMAL", "12345.6789"},
		{"money keeps its exact text", []byte("-0.5000"), "MONEY", "-0.5000"},
		{
			"uniqueidentifier is byte-swapped into canonical form",
			[]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
			"UNIQUEIDENTIFIER",
			"03020100-0504-0706-0809-0A0B0C0D0E0F",
		},
		{"varbinary is hex", []byte{0xde, 0xad, 0xbe, 0xef}, "VARBINARY", "0xDEADBEEF"},

		// Unknown types that arrive as bytes.
		{"unknown but printable becomes text", []byte("hello"), "SQL_VARIANT", "hello"},
		{"unknown and not utf-8 becomes hex", []byte{0xff, 0xfe}, "SQL_VARIANT", "0xFFFE"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := convert(tc.in, tc.typeName); got != tc.want {
				t.Errorf("convert(%v, %q) = %#v, want %#v", tc.in, tc.typeName, got, tc.want)
			}
		})
	}
}

// TestConvertDecimalIsNotFloat is the reason convert takes a type name at all.
// Rendering a decimal through float64 is how a payroll figure quietly changes.
func TestConvertDecimalIsNotFloat(t *testing.T) {
	const exact = "123456789012345678.99"
	if got := convert([]byte(exact), "DECIMAL"); got != exact {
		t.Errorf("convert() = %v, want the exact text %q", got, exact)
	}
}

func TestFormatTime(t *testing.T) {
	// datetime2 has no offset; the driver surfaces it as UTC. Rendering a "Z"
	// would assert a time zone the database never stated.
	naive := time.Date(2026, 7, 29, 13, 45, 30, 123000000, time.UTC)
	if got := formatTime(naive); got != "2026-07-29T13:45:30.123" {
		t.Errorf("formatTime(naive) = %q", got)
	}

	// datetimeoffset does carry one, so it keeps it.
	off := time.Date(2026, 7, 29, 13, 45, 30, 0, time.FixedZone("CST", 8*3600))
	if got := formatTime(off); got != "2026-07-29T13:45:30+08:00" {
		t.Errorf("formatTime(offset) = %q", got)
	}
}

func TestLimitsWithDefaults(t *testing.T) {
	got := Limits{}.withDefaults()
	if got.MaxRows != 500 || got.MaxBytes != 1<<20 {
		t.Errorf("row/byte defaults = %d/%d", got.MaxRows, got.MaxBytes)
	}
	if got.Timeout != 30*time.Second || got.LockTimeout != 5*time.Second {
		t.Errorf("timeout defaults = %v/%v", got.Timeout, got.LockTimeout)
	}
	// A negative lock timeout is "wait forever" and must survive defaulting,
	// otherwise asking for it would silently get five seconds.
	if got := (Limits{LockTimeout: -1}).withDefaults(); got.LockTimeout != -1 {
		t.Errorf("LockTimeout = %v, want -1 preserved", got.LockTimeout)
	}
}
