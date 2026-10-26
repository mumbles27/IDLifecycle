//go:build !windows

package sources

// The ODBC driver (github.com/alexbrainman/odbc) needs cgo and unixODBC off Windows, and the
// service is built with cgo disabled, so db2/odbc sources report a clear error instead.
const odbcAvailable = false

func ansiToUTF8(b []byte) string { return string(b) }

func windowsACP() uint32 { return 0 }
