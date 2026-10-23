//go:build windows

package secrets

import (
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
	if len(in) == 0 {
		return nil, windows.ERROR_INVALID_PARAMETER
	}
	inBlob := windows.DataBlob{Size: uint32(len(in)), Data: &in[0]}
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
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	res := make([]byte, out.Size)
	copy(res, unsafe.Slice(out.Data, out.Size))
	return res, nil
}
