// Package spaudit compares three views of the same stored procedures: the
// scripts on disk, the definitions in the database, and the call sites in the
// Java source.
//
// Two-way comparison is what people usually build, and it cannot see the
// failure that actually hurts. Files against the database tells you the
// scripts are stale. The database against Java tells you something is
// uncalled. Only all three together surface a procedure the application will
// ask for and the server does not have — a ghost. Nothing detects that today
// except a user hitting the screen that calls it, in production, at month end.
//
// The classification deliberately reports one status per procedure while
// keeping the raw presence flags visible in the output. A single status makes
// the report skimmable; the flags make it checkable. Precedence is documented
// on Classify, because "which label wins" is the one thing a reader must not
// have to guess.
package spaudit
