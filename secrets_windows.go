//go:build windows

package main

import (
	"errors"
	"syscall"
	"unsafe"
)

var (
	crypt32          = syscall.NewLazyDLL("crypt32.dll")
	procProtectData  = crypt32.NewProc("CryptProtectData")
	procUnprotect    = crypt32.NewProc("CryptUnprotectData")
	procLocalFreeSec = syscall.NewLazyDLL("kernel32.dll").NewProc("LocalFree")
)

type dataBlob struct {
	cb   uint32
	data *byte
}

func blobOf(b []byte) dataBlob {
	if len(b) == 0 {
		return dataBlob{}
	}
	return dataBlob{cb: uint32(len(b)), data: &b[0]}
}

func dpapi(proc *syscall.LazyProc, in []byte) ([]byte, error) {
	src := blobOf(in)
	var out dataBlob
	r, _, e := proc.Call(uintptr(unsafe.Pointer(&src)), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, errors.New("DPAPI: " + e.Error())
	}
	defer procLocalFreeSec.Call(uintptr(unsafe.Pointer(out.data)))
	res := make([]byte, out.cb)
	copy(res, unsafe.Slice(out.data, out.cb))
	return res, nil
}

func protectSecret(b []byte) ([]byte, error)   { return dpapi(procProtectData, b) }
func unprotectSecret(b []byte) ([]byte, error) { return dpapi(procUnprotect, b) }
