//go:build !windows

package store

// The ODBC driver needs cgo and unixODBC off Windows and the service is built with cgo
// disabled, so the db2 store is unavailable here and Open reports a clear error.
const odbcAvailable = false
