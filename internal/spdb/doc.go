// Package spdb reads stored-procedure definitions from the database.
//
// This is the third leg of the audit. The files on disk say what someone
// intended, the Java call sites say what the application asks for, and only
// this package can say what the server will actually run. Without it a "diff"
// is just two guesses compared against each other.
//
// Definitions are read from sys.sql_modules rather than sp_helptext because
// sp_helptext returns the text in 255-character chunks that must be
// reassembled — a step that silently loses long lines, which stored procedures
// full of Chinese comments have plenty of.
package spdb
