//go:build windows

package secrets

import (
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
)

func init() {
	protect = func(b []byte) ([]byte, error) { return dpapi(b, true) }
	unprotect = func(b []byte) ([]byte, error) { return dpapi(b, false) }
}

// dpapi uses machine scope so the key is readable by the service account and by an
// administrator running `idlifecycled` CLI commands on the same host, but not off-box.
func dpapi(in []byte, encrypt bool) ([]byte, error) {
	if len(in) == 0 || len(in) > math.MaxUint32 {
		return nil, windows.ERROR_INVALID_PARAMETER
	}
	inBlob := windows.DataBlob{Size: uint32(len(in)), Data: &in[0]} // #nosec G115 -- 1..MaxUint32, checked above
	var out windows.DataBlob
	var err error
	const flags = windows.CRYPTPROTECT_UI_FORBIDDEN | windows.CRYPTPROTECT_LOCAL_MACHINE
	if encrypt {
		err = windows.CryptProtectData(&inBlob, nil, nil, 0, nil, flags, &out)
	} else {
		err = windows.CryptUnprotectData(&inBlob, nil, nil, 0, nil, flags, &out)
	}
	if err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data))) // #nosec G103 -- frees the buffer DPAPI allocated, as the API requires
	res := make([]byte, out.Size)
	copy(res, unsafe.Slice(out.Data, out.Size)) // #nosec G103 -- copies the DPAPI output buffer of the size DPAPI returned
	return res, nil
}
