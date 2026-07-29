// Package schemadict parses the project's data dictionary and searches it.
//
// The dictionary is worth having because the database alone cannot answer the
// question people actually ask. HRM's columns are named leave_d_days,
// rpr_notes, ept_no; the catalog gives their types and nothing else. The
// dictionary carries the Chinese description that says leave_d_days is
// 事假日時數 — which is the only way to get from "where are personal-leave
// hours recorded" to a column name.
//
// It is a generated file, and parts of it are wrong. Measured on HRM's copy:
//
//   - The "## 欄位說明" prose section marks every column （主鍵）, including
//     all thirty columns of ADVANCE_BONUS_GRANT. It is template output, not
//     data, so this package ignores that section entirely and reads only the
//     "## 欄位定義" table.
//   - Missing values appear as the literal "nan", left over from whatever
//     produced the file. They are normalised to empty here, because a search
//     result reading 備註: nan is worse than one reading nothing.
//   - The 主鍵 column is marked with V, v, Y, PK, 1 and 2 across 262 of 3570
//     columns. The vocabulary is inconsistent enough that this package reports
//     only whether a marker is present, never what it meant.
//
// None of this is corrected, only reported. A dictionary that quietly invents
// the missing half would be more dangerous than one with visible gaps: the
// caller can verify a column against the live catalog, but only if it knows
// the claim came from a document rather than from the server.
package schemadict
