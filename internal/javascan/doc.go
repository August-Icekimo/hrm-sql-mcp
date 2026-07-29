// Package javascan finds the stored procedures a Java source tree calls.
//
// Two filters do the work, and both exist because of what the output is used
// for.
//
// It reads string literals only. A plain grep for sp_[a-z0-9_]* over the same
// tree also matches the identifier in "// TODO: retire sp_ema0100", in a
// commented-out block, and in a Java field named sp_amount — and those false
// positives all land in the "Java calls it but the database does not have it"
// bucket, which is the one row type an operator is supposed to treat as an
// incident.
//
// Within those literals it separates call position (EXEC, EXECUTE, {call ...})
// from a bare mention. Literals alone are not enough: measured against HRM,
// WDC0900Action builds a SELECT whose column alias is sp_amount, which reads
// as a procedure name by every rule except the one that matters. Only call
// position raises an alarm; a mention is enough to say a procedure is not dead,
// which is the weaker claim and can take the weaker evidence.
//
// Neither filter makes the result complete, and the package does not pretend
// otherwise: a name assembled at runtime ("sp_ema" + code) is invisible unless
// some part of it survives as a literal. This is a floor on what is called,
// never a ceiling — so a procedure reported as uncalled is a candidate for
// review, not a deletion order.
package javascan
