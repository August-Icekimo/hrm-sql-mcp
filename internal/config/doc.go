// Package config loads process settings from the environment and database
// credentials from a private file.
//
// Credentials are deliberately not read from the consuming project's .env.
// For HRM that file holds the application's pooled login, gets written into
// Tomcat's setenv.sh by the deploy script, and is therefore readable by any
// code running inside the webapp. Reusing it would also make a DBA's "who
// changed this row" unanswerable, because every write would carry the
// application's identity.
package config
