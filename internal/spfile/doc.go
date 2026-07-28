// Package spfile reads the loose stored-procedure scripts a project keeps on
// disk and compares them against what the database actually has.
//
// The hard part is not the diff, it is the decoding. HRM's "Stored Procedure"
// directory holds 149 files accumulated over more than a decade, and they are
// not in one encoding: measured, 105 are UTF-16LE with a BOM (SSMS's default
// output), 12 are UTF-8, and the rest are CP950/Big5 with Chinese comments.
//
// Guessing wrong is worse than having no tool at all. A misdecoded file turns
// its Chinese comments into replacement characters, the diff then reports the
// whole procedure as changed, and every row of the resulting report becomes
// untrustworthy — while still looking authoritative. So Decode reports which
// encoding it used and returns an error rather than guessing when it cannot
// tell.
package spfile
