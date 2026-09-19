// Package winusb opens an Android device's ADB interface directly through the
// WinUSB driver, with no adb.exe and no cgo.
//
// Windows binds the ADB interface to Microsoft's generic WINUSB driver via the
// device's MS OS descriptors, which also publish the interface GUID below. That
// makes the bulk endpoints reachable from user space without installing
// anything.
//
// The interface can only be claimed by one process at a time, so the adb server
// must not be running.
package winusb

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

// InterfaceGUID identifies the ADB interface. Android publishes this same GUID
// on every device that exposes adb over WinUSB.
var InterfaceGUID = syscall.GUID{
	Data1: 0xF72FE0D4,
	Data2: 0xCBCB,
	Data3: 0x407D,
	Data4: [8]byte{0x88, 0x14, 0x9E, 0xD6, 0x73, 0xD0, 0xDD, 0x6B},
}

var (
	setupapi = syscall.NewLazyDLL("setupapi.dll")
	winusb   = syscall.NewLazyDLL("winusb.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procGetClassDevsW             = setupapi.NewProc("SetupDiGetClassDevsW")
	procEnumDeviceInterfaces      = setupapi.NewProc("SetupDiEnumDeviceInterfaces")
	procGetDeviceInterfaceDetailW = setupapi.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procDestroyDeviceInfoList     = setupapi.NewProc("SetupDiDestroyDeviceInfoList")

	procInitialize            = winusb.NewProc("WinUsb_Initialize")
	procFree                  = winusb.NewProc("WinUsb_Free")
	procQueryInterfaceSetting = winusb.NewProc("WinUsb_QueryInterfaceSettings")
	procQueryPipe             = winusb.NewProc("WinUsb_QueryPipe")
	procReadPipe              = winusb.NewProc("WinUsb_ReadPipe")
	procWritePipe             = winusb.NewProc("WinUsb_WritePipe")
	procSetPipePolicy         = winusb.NewProc("WinUsb_SetPipePolicy")
	procGetOverlappedResult   = winusb.NewProc("WinUsb_GetOverlappedResult")
	procResetPipe             = winusb.NewProc("WinUsb_ResetPipe")
	procFlushPipe             = winusb.NewProc("WinUsb_FlushPipe")

	procCancelIoEx  = kernel32.NewProc("CancelIoEx")
	procCreateEvent = kernel32.NewProc("CreateEventW")
	procResetEvent  = kernel32.NewProc("ResetEvent")
)

const (
	digcfPresent         = 0x02
	digcfDeviceInterface = 0x10

	errNoMoreItems = 259
	errIOPending   = 997

	// Errnos that all mean the same thing in practice: something else owns the
	// interface. CreateFile reports the first two, WinUsb_Initialize the rest.
	errAccessDenied     = 5
	errSharingViolation = 32
	errNotSupported     = 50
	errBusy             = 170

	usbdPipeTypeBulk = 2

	// Pipe policy identifiers.
	policyPipeTransferTimeout = 0x03
	policyAutoClearStall      = 0x01
)

type spDeviceInterfaceData struct {
	cbSize             uint32
	interfaceClassGUID syscall.GUID
	flags              uint32
	reserved           uintptr
}

type usbInterfaceDescriptor struct {
	Length            uint8
	DescriptorType    uint8
	InterfaceNumber   uint8
	AlternateSetting  uint8
	NumEndpoints      uint8
	InterfaceClass    uint8
	InterfaceSubClass uint8
	InterfaceProtocol uint8
	Interface         uint8
}

type pipeInformation struct {
	PipeType      uint32
	PipeID        uint8
	_             uint8
	MaxPacketSize uint16
	Interval      uint8
	_             [3]uint8
}

// DevicePaths lists every present ADB interface.
func DevicePaths() ([]string, error) {
	guid := InterfaceGUID
	h, _, err := procGetClassDevsW.Call(
		uintptr(unsafe.Pointer(&guid)), 0, 0, digcfPresent|digcfDeviceInterface)
	if h == uintptr(syscall.InvalidHandle) {
		return nil, fmt.Errorf("SetupDiGetClassDevs: %w", err)
	}
	defer procDestroyDeviceInfoList.Call(h)

	var paths []string
	for i := uint32(0); ; i++ {
		var did spDeviceInterfaceData
		did.cbSize = uint32(unsafe.Sizeof(did))
		r, _, e := procEnumDeviceInterfaces.Call(
			h, 0, uintptr(unsafe.Pointer(&guid)), uintptr(i), uintptr(unsafe.Pointer(&did)))
		if r == 0 {
			if errno, ok := e.(syscall.Errno); ok && errno == errNoMoreItems {
				break
			}
			break
		}

		// First call sizes the buffer, second fills it.
		var needed uint32
		procGetDeviceInterfaceDetailW.Call(h, uintptr(unsafe.Pointer(&did)), 0, 0,
			uintptr(unsafe.Pointer(&needed)), 0)
		if needed == 0 {
			continue
		}
		buf := make([]byte, needed)
		// SP_DEVICE_INTERFACE_DETAIL_DATA_W starts with a DWORD cbSize that
		// must describe the fixed part of the struct, which is 8 on 64-bit
		// because of alignment, not the size of the whole buffer.
		*(*uint32)(unsafe.Pointer(&buf[0])) = 8
		r, _, e = procGetDeviceInterfaceDetailW.Call(h, uintptr(unsafe.Pointer(&did)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(needed),
			uintptr(unsafe.Pointer(&needed)), 0)
		if r == 0 {
			return nil, fmt.Errorf("SetupDiGetDeviceInterfaceDetail: %w", e)
		}
		// The path is a NUL-terminated UTF-16 string right after cbSize.
		chars := (*[1 << 16]uint16)(unsafe.Pointer(&buf[4]))[: (needed-4)/2 : (needed-4)/2]
		paths = append(paths, syscall.UTF16ToString(chars))
	}
	return paths, nil
}

// ErrInUse reports that another process already holds the ADB interface.
//
// It is worth distinguishing because it is not a transient condition: waiting
// will not help, unlike the interface briefly vanishing while adbd re-exposes
// it.
var ErrInUse = errors.New("the ADB interface is held by another process")

// inUseError reports a failed claim in a way DialTimeout can recognise.
//
// Both the sentinel and the underlying errno are wrapped, because the retry
// loop matches on ErrInUse while the message still has to name the real Windows
// error. Wrapping only the errno, as this used to, made the ErrInUse check dead
// code and cost the user the full retry timeout in the single most common
// failure case: a running adb server.
func inUseError(stage string, err error) error {
	return fmt.Errorf("cannot claim the ADB interface (%s): %w: %w\n"+
		"another process already holds it - stop the adb server "+
		"(adb kill-server) and close Android Studio or scrcpy", stage, ErrInUse, err)
}

// inUse reports whether an errno means the interface is already owned.
func inUse(err error, codes ...syscall.Errno) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	for _, c := range codes {
		if errno == c {
			return true
		}
	}
	return false
}

// Device is one claimed ADB interface.
//
// Ordering contract for the handles: every transfer and every CancelIO holds
// hmu for reading, and Close takes it for writing. Close therefore waits for
// any transfer still in flight instead of freeing the handle underneath it,
// which is what an Abort racing a Close used to do: CancelIoEx and WinUsb_Read
// could both be handed a handle CloseHandle had already released. Cancel first,
// then close.
//
// One reader and one writer at a time: each direction has a single OVERLAPPED
// and a single event, so two concurrent reads would overwrite each other's.
// adbproto guarantees this, with one readLoop and a mutex around every write.
type Device struct {
	Path string

	hmu     sync.RWMutex
	closed  bool
	file    syscall.Handle
	iface   uintptr // WINUSB_INTERFACE_HANDLE
	inPipe  uint8
	outPipe uint8

	readOv  *overlapped
	writeOv *overlapped
}

type overlapped struct {
	ov    syscall.Overlapped
	event syscall.Handle
}

// createEvent makes a manual-reset event. The stdlib syscall package does not
// expose CreateEvent on Windows, so it comes straight from kernel32.
func createEvent() (syscall.Handle, error) {
	h, _, e := procCreateEvent.Call(0, 1, 0, 0)
	if h == 0 {
		return 0, fmt.Errorf("CreateEvent: %w", e)
	}
	return syscall.Handle(h), nil
}

func newOverlapped() (*overlapped, error) {
	ev, err := createEvent()
	if err != nil {
		return nil, err
	}
	o := &overlapped{event: ev}
	o.ov.HEvent = ev
	return o, nil
}

// Open claims the interface at path and locates its bulk endpoints.
func Open(path string) (*Device, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// WinUSB requires the handle be opened for overlapped I/O.
	fh, err := syscall.CreateFile(p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		if inUse(err, errAccessDenied, errSharingViolation) {
			return nil, inUseError("CreateFile", err)
		}
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	d := &Device{Path: path, file: fh}
	r, _, e := procInitialize.Call(uintptr(fh), uintptr(unsafe.Pointer(&d.iface)))
	if r == 0 {
		syscall.CloseHandle(fh)
		// CreateFile can succeed on a handle whose interface is already claimed;
		// the refusal then shows up here instead, with an errno that varies by
		// Windows build.
		if inUse(e, errBusy, errAccessDenied, errNotSupported) {
			return nil, inUseError("WinUsb_Initialize", e)
		}
		return nil, fmt.Errorf("WinUsb_Initialize: %w", e)
	}

	if err := d.findBulkPipes(); err != nil {
		d.Close()
		return nil, err
	}
	if d.readOv, err = newOverlapped(); err != nil {
		d.Close()
		return nil, err
	}
	if d.writeOv, err = newOverlapped(); err != nil {
		d.Close()
		return nil, err
	}

	// RAW_IO is deliberately left off: it requires every read buffer to be a
	// multiple of the endpoint's maximum packet size, which cannot express the
	// adb framing's 24-byte header read.
	// Discard anything a previous session left in the pipes. Without this a
	// reconnect reads the tail of the last conversation and every adb header
	// after it is misaligned.
	d.resetPipes()

	one := uint8(1)
	// A stalled pipe should clear itself rather than wedging the connection.
	procSetPipePolicy.Call(d.iface, uintptr(d.inPipe), policyAutoClearStall, 1,
		uintptr(unsafe.Pointer(&one)))
	procSetPipePolicy.Call(d.iface, uintptr(d.outPipe), policyAutoClearStall, 1,
		uintptr(unsafe.Pointer(&one)))
	return d, nil
}

// resetPipes drops any data buffered in the driver or the device's endpoints,
// so a new connection starts on a packet boundary.
func (d *Device) resetPipes() {
	for _, pipe := range []uint8{d.inPipe, d.outPipe} {
		procResetPipe.Call(d.iface, uintptr(pipe))
		procFlushPipe.Call(d.iface, uintptr(pipe))
	}
}

func (d *Device) findBulkPipes() error {
	var desc usbInterfaceDescriptor
	r, _, e := procQueryInterfaceSetting.Call(d.iface, 0, uintptr(unsafe.Pointer(&desc)))
	if r == 0 {
		return fmt.Errorf("WinUsb_QueryInterfaceSettings: %w", e)
	}
	var in, out uint8
	for i := uint8(0); i < desc.NumEndpoints; i++ {
		var pi pipeInformation
		r, _, e := procQueryPipe.Call(d.iface, 0, uintptr(i), uintptr(unsafe.Pointer(&pi)))
		if r == 0 {
			return fmt.Errorf("WinUsb_QueryPipe(%d): %w", i, e)
		}
		if pi.PipeType != usbdPipeTypeBulk {
			continue
		}
		// Bit 7 of the endpoint address marks the IN direction.
		if pi.PipeID&0x80 != 0 {
			in = pi.PipeID
		} else {
			out = pi.PipeID
		}
	}
	if in == 0 || out == 0 {
		return fmt.Errorf("no bulk endpoint pair on the interface (in=%#x out=%#x)", in, out)
	}
	d.inPipe, d.outPipe = in, out
	return nil
}

// Endpoints reports the discovered bulk endpoint addresses.
func (d *Device) Endpoints() (in, out uint8) { return d.inPipe, d.outPipe }

// Read fills p from the device's bulk IN endpoint.
func (d *Device) Read(p []byte) (int, error) { return d.transfer(false, p) }

// Write sends p to the device's bulk OUT endpoint.
func (d *Device) Write(p []byte) (int, error) { return d.transfer(true, p) }

// ErrClosed reports use of a device that has already been released.
var ErrClosed = errors.New("the USB device is closed")

func (d *Device) transfer(out bool, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Held for the whole transfer, including the overlapped wait: the handles
	// and the event must stay alive until the driver is done with them.
	d.hmu.RLock()
	defer d.hmu.RUnlock()
	if d.closed {
		return 0, ErrClosed
	}
	proc, pipe, o := procReadPipe, d.inPipe, d.readOv
	if out {
		proc, pipe, o = procWritePipe, d.outPipe, d.writeOv
	}
	var transferred uint32
	// The driver keeps writing into p and transferred for as long as the
	// transfer is pending, which outlasts the call that started it. Nothing
	// below reads either through a Go reference - they reach the driver as
	// uintptr - so without this the collector is entitled to reclaim them while
	// the device is still filling them in.
	defer runtime.KeepAlive(p)
	defer runtime.KeepAlive(&transferred)

	procResetEvent.Call(uintptr(o.event))
	r, _, e := proc.Call(d.iface, uintptr(pipe),
		uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)),
		uintptr(unsafe.Pointer(&transferred)), uintptr(unsafe.Pointer(&o.ov)))
	if r != 0 {
		return int(transferred), nil
	}
	errno, _ := e.(syscall.Errno)
	if errno != errIOPending {
		return 0, fmt.Errorf("usb transfer: %w", e)
	}
	r, _, e = procGetOverlappedResult.Call(d.iface,
		uintptr(unsafe.Pointer(&o.ov)), uintptr(unsafe.Pointer(&transferred)), 1)
	if r == 0 {
		return 0, fmt.Errorf("usb transfer result: %w", e)
	}
	return int(transferred), nil
}

// ReadFull reads exactly len(p) bytes, reassembling across USB packets.
func (d *Device) ReadFull(p []byte) error {
	for off := 0; off < len(p); {
		n, err := d.Read(p[off:])
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("usb read returned no data")
		}
		off += n
	}
	return nil
}

// WriteAll writes every byte of p.
func (d *Device) WriteAll(p []byte) error {
	for off := 0; off < len(p); {
		n, err := d.Write(p[off:])
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("usb write made no progress")
		}
		off += n
	}
	return nil
}

// SetTimeout bounds how long a single read may block, in milliseconds. Zero
// means wait indefinitely.
//
// Nothing uses this, on purpose. A pipe transfer timeout abandons a bulk
// transfer in the middle of an adb message, which loses the bytes already
// received and leaves the framing misaligned for every message after it. Use
// CancelIO to unblock a wedged reader instead, and give up on the connection.
func (d *Device) SetTimeout(ms uint32) {
	d.hmu.RLock()
	defer d.hmu.RUnlock()
	if d.closed {
		return
	}
	procSetPipePolicy.Call(d.iface, uintptr(d.inPipe), policyPipeTransferTimeout, 4,
		uintptr(unsafe.Pointer(&ms)))
}

// CancelIO aborts any transfer currently blocked on this device, so a reader
// goroutine can be unblocked before the handles are freed.
//
// It takes the read lock rather than the write lock so that it can run while a
// transfer is parked in the driver, which is the only moment it is useful. That
// also makes it safe after Close: the handle cannot be freed while this holds
// the lock, and once it is freed the closed flag stops the call.
func (d *Device) CancelIO() {
	d.hmu.RLock()
	defer d.hmu.RUnlock()
	if d.closed || d.file == 0 {
		return
	}
	procCancelIoEx.Call(uintptr(d.file), 0)
}

// Close releases the interface, after waiting for any transfer still in flight.
// Cancel first: a read with nothing to receive blocks forever, and Close blocks
// with it rather than freeing a handle the driver still holds.
//
// It is idempotent, so an aborted connection can still be closed normally.
func (d *Device) Close() error {
	d.hmu.Lock()
	defer d.hmu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	if d.iface != 0 {
		d.resetPipes()
		procFree.Call(d.iface)
		d.iface = 0
	}
	if d.readOv != nil {
		syscall.CloseHandle(d.readOv.event)
		d.readOv = nil
	}
	if d.writeOv != nil {
		syscall.CloseHandle(d.writeOv.event)
		d.writeOv = nil
	}
	if d.file != 0 {
		syscall.CloseHandle(d.file)
		d.file = 0
	}
	return nil
}
