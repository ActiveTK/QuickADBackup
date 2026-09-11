// Package singleton stops a second copy of the tool from starting.
//
// Only one process can hold the phone's USB interface, so a second instance
// cannot do anything useful. Catching it here gives a message that names the
// real cause, instead of a generic "the interface is in use" from the driver.
package singleton

import (
	"errors"
	"syscall"
	"unsafe"
)

// ErrAlreadyRunning reports that another QuickADBackup process holds the lock.
var ErrAlreadyRunning = errors.New("QuickADBackup is already running")

// name is shared by the GUI and the command line, because either one holds the
// device to the exclusion of the other.
const name = "QuickADBackup.device.lock"

const errAlreadyExists = 183

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutex  = kernel32.NewProc("CreateMutexW")
	procReleaseMutex = kernel32.NewProc("ReleaseMutex")
)

// Lock is a held single-instance claim.
type Lock struct{ h syscall.Handle }

// Acquire claims the lock, or returns ErrAlreadyRunning.
//
// The mutex lives in the session namespace, which is the right scope: a
// different desktop session has its own device handles.
func Acquire() (*Lock, error) {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, _, e := procCreateMutex.Call(0, 1, uintptr(unsafe.Pointer(p)))
	if h == 0 {
		return nil, e
	}
	if errno, ok := e.(syscall.Errno); ok && errno == errAlreadyExists {
		syscall.CloseHandle(syscall.Handle(h))
		return nil, ErrAlreadyRunning
	}
	return &Lock{h: syscall.Handle(h)}, nil
}

// Release drops the claim. Windows releases it anyway if the process dies, so
// a crash cannot lock the user out.
func (l *Lock) Release() {
	if l == nil || l.h == 0 {
		return
	}
	procReleaseMutex.Call(uintptr(l.h))
	syscall.CloseHandle(l.h)
	l.h = 0
}
