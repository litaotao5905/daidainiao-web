//go:build windows

package main

import (
	"errors"
	"syscall"
	"unsafe"
)

// winKnownDocuments asks the shell for FOLDERID_Documents (handles OneDrive
// backup and relocated folders).
func winKnownDocuments() (string, error) {
	guid := syscall.GUID{Data1: 0xFDD39AD0, Data2: 0x238F, Data3: 0x46AF, Data4: [8]byte{0xAD, 0xB4, 0x6C, 0x85, 0x48, 0x03, 0x69, 0xC7}}
	proc := syscall.NewLazyDLL("shell32.dll").NewProc("SHGetKnownFolderPath")
	var out *uint16
	hr, _, _ := proc.Call(uintptr(unsafe.Pointer(&guid)), 0, 0, uintptr(unsafe.Pointer(&out)))
	if hr != 0 || out == nil {
		return "", errors.New("SHGetKnownFolderPath failed")
	}
	defer syscall.NewLazyDLL("ole32.dll").NewProc("CoTaskMemFree").Call(uintptr(unsafe.Pointer(out)))
	n := 0
	for p := unsafe.Pointer(out); *(*uint16)(p) != 0 && n < 32768; n++ {
		p = unsafe.Add(p, 2)
	}
	return syscall.UTF16ToString(unsafe.Slice(out, n)), nil
}
