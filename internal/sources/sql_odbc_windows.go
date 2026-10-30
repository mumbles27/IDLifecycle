//go:build windows

package sources

import (
	"math"
	"unicode/utf16"

	"golang.org/x/sys/windows"

	// IBM Db2 and generic ODBC sources: database/sql driver "odbc". On Windows it calls the ODBC
	// driver manager (odbc32.dll) through syscall, without cgo. Its non-Windows build needs cgo and
	// unixODBC, so it is only linked here.
	_ "github.com/alexbrainman/odbc"
)

const odbcAvailable = true

const cpACP = 0 // CP_ACP

// ansiToUTF8 decodes text in the process's ANSI code page (CP_ACP), which is what ODBC drivers
// return for SQL_C_CHAR data on Windows.
func ansiToUTF8(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if len(b) > math.MaxInt32 {
		return string(b)
	}
	size := int32(len(b)) // #nosec G115 -- at most MaxInt32, checked above
	n, err := windows.MultiByteToWideChar(cpACP, 0, &b[0], size, nil, 0)
	if err != nil || n <= 0 {
		return string(b)
	}
	w := make([]uint16, n)
	n, err = windows.MultiByteToWideChar(cpACP, 0, &b[0], size, &w[0], n)
	if err != nil || n <= 0 {
		return string(b)
	}
	return string(utf16.Decode(w[:n]))
}

func windowsACP() uint32 { return windows.GetACP() }
