package spfile

import (
	"bytes"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/traditionalchinese"
)

// Encoding names reported by Decode.
const (
	EncUTF16LE = "utf-16le"
	EncUTF16BE = "utf-16be"
	EncUTF8BOM = "utf-8-bom"
	EncUTF8    = "utf-8"
	EncCP950   = "cp950"
)

// Decode converts a script file's bytes to text and reports which encoding was
// used.
//
// The order is BOM, then UTF-8 validation, then CP950, and it matters:
//
//   - A BOM is a statement of fact by whoever wrote the file, so it wins.
//     SSMS writes UTF-16LE with a BOM by default, which is why 105 of the 124
//     .sql files in HRM are that.
//   - Without a BOM, valid UTF-8 is almost certainly UTF-8. The false-positive
//     risk is negligible: CP950 text containing Chinese will essentially never
//     also be valid UTF-8.
//   - Everything else is treated as CP950, which is the only other encoding
//     this corpus contains.
//
// If the result still contains replacement characters after decoding, that is
// reported as an error rather than returned as text. Silently producing
// mojibake would make every downstream diff wrong in a way that looks right.
func Decode(raw []byte) (text string, encoding string, err error) {
	switch {
	case bytes.HasPrefix(raw, []byte{0xFF, 0xFE}):
		return decodeUTF16(raw[2:], false), EncUTF16LE, nil
	case bytes.HasPrefix(raw, []byte{0xFE, 0xFF}):
		return decodeUTF16(raw[2:], true), EncUTF16BE, nil
	case bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}):
		return string(raw[3:]), EncUTF8BOM, nil
	}

	if utf8.Valid(raw) {
		return string(raw), EncUTF8, nil
	}

	dec := traditionalchinese.Big5.NewDecoder()
	out, err := dec.Bytes(raw)
	if err != nil {
		return "", "", fmt.Errorf("not UTF-8 and not decodable as CP950: %w", err)
	}
	s := string(out)
	if i := indexReplacement(s); i >= 0 {
		return "", "", fmt.Errorf(
			"decoded as CP950 but produced a replacement character at offset %d; "+
				"the encoding could not be determined reliably", i)
	}
	return s, EncCP950, nil
}

// decodeUTF16 converts UTF-16 code units to a Go string.
//
// An odd trailing byte is dropped rather than treated as an error: a few of
// these files were produced by tools that padded, and losing one byte at EOF
// has never changed a procedure body. Anything that mattered would show up in
// the diff.
func decodeUTF16(b []byte, bigEndian bool) string {
	if len(b)%2 == 1 {
		b = b[:len(b)-1]
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if bigEndian {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	return string(utf16.Decode(u))
}

func indexReplacement(s string) int {
	for i, r := range s {
		if r == utf8.RuneError {
			return i
		}
	}
	return -1
}
