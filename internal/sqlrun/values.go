package sqlrun

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	mssql "github.com/microsoft/go-mssqldb"
)

// convert turns a driver value into something that survives JSON encoding and
// still says what the database said.
//
// The column type name is needed because go-mssqldb hands back []byte for
// three unrelated families: DECIMAL/MONEY (ASCII digits), UNIQUEIDENTIFIER
// (16 raw bytes in SQL Server's mixed-endian order), and the genuinely binary
// types. Rendering all of them the same way would either hex-encode a salary
// figure or print a GUID as mojibake.
func convert(v any, typeName string) any {
	switch x := v.(type) {
	case nil:
		return nil
	case time.Time:
		return formatTime(x)
	case []byte:
		return convertBytes(x, typeName)
	default:
		return v
	}
}

func convertBytes(b []byte, typeName string) any {
	switch strings.ToUpper(typeName) {
	case "DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY":
		// Already the decimal's exact text. Converting to float64 here would
		// lose precision on exactly the columns where precision is the point.
		return string(b)
	case "UNIQUEIDENTIFIER":
		var u mssql.UniqueIdentifier
		if err := u.Scan(b); err == nil {
			return u.String()
		}
		return hexString(b)
	case "BINARY", "VARBINARY", "IMAGE", "TIMESTAMP", "ROWVERSION":
		return hexString(b)
	}
	// Unknown type that arrived as bytes: printable text is far more useful as
	// text, and anything else must not be mangled into invalid UTF-8.
	if utf8.Valid(b) {
		return string(b)
	}
	return hexString(b)
}

func hexString(b []byte) string { return "0x" + strings.ToUpper(hex.EncodeToString(b)) }

// formatTime renders a timestamp without inventing a time zone.
//
// SQL Server's datetime/datetime2 carry no offset, and the driver surfaces
// them as UTC. Formatting those as RFC 3339 would append a "Z" that asserts
// something the database never said — a real problem for HRM, where attendance
// timestamps are local wall-clock time. Only datetimeoffset, which does carry
// an offset, is rendered with one.
func formatTime(t time.Time) string {
	if _, off := t.Zone(); off == 0 {
		return t.Format("2006-01-02T15:04:05.999999999")
	}
	return t.Format(time.RFC3339Nano)
}

// sizeOf estimates the encoded size of one converted value, for the byte cap.
//
// An estimate is the right tool here: the cap exists to keep a response
// absorbable, so being a few bytes out per row does not matter, while walking
// the value through a real encoder for every cell would cost more than the
// query.
func sizeOf(v any) int {
	switch x := v.(type) {
	case nil:
		return 4
	case string:
		return len(x)
	case bool:
		return 5
	case []byte:
		return len(x)
	default:
		return len(fmt.Sprint(x))
	}
}
