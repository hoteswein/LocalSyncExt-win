//go:build windows

package main

import (
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"
)

var procGetVolumeInformation = syscall.NewLazyDLL("kernel32.dll").NewProc("GetVolumeInformationW")

// volumeID возвращает серийный номер тома, на котором лежит путь ("" если определить нельзя).
func volumeID(dir string) string {
	vn := filepath.VolumeName(dir)
	if vn == "" {
		return ""
	}
	p, err := syscall.UTF16PtrFromString(vn + `\`)
	if err != nil {
		return ""
	}
	var serial uint32
	r, _, _ := procGetVolumeInformation.Call(uintptr(unsafe.Pointer(p)), 0, 0, uintptr(unsafe.Pointer(&serial)), 0, 0, 0, 0)
	if r == 0 {
		return ""
	}
	return fmt.Sprintf("%08X", serial)
}
