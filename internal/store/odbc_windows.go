//go:build windows

package store

// IBM Db2 store: database/sql driver "odbc". On Windows it calls the ODBC driver manager
// (odbc32.dll) through syscall, without cgo. Its non-Windows build needs cgo and unixODBC,
// so it is only linked here.
import _ "github.com/alexbrainman/odbc"

const odbcAvailable = true
