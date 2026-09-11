// Package adbproto speaks the ADB wire protocol directly to a device, with no
// adb.exe and no adb server.
//
// The protocol is documented in AOSP under packages/modules/adb: protocol.txt
// describes the framing implemented here, SERVICES.TXT the service names, and
// SYNC.TXT the file-transfer service in sync.go.
//
// Nothing in this package writes to the device: only read-only services are
// used, and the sync layer deliberately omits SEND.
package adbproto

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"quickadbackup/internal/winusb"
)

// Message commands, little-endian ASCII as they appear on the wire.
const (
	cmdCNXN = 0x4e584e43 // CNXN: connect
	cmdAUTH = 0x48545541 // AUTH: authenticate
	cmdOPEN = 0x4e45504f // OPEN: open a stream
	cmdOKAY = 0x59414b4f // OKAY: ready / acknowledged
	cmdCLSE = 0x45534c43 // CLSE: close a stream
	cmdWRTE = 0x45545257 // WRTE: stream payload
)

// AUTH payload types.
const (
	authToken        = 1
	authSignature    = 2
	authRSAPublicKey = 3
)

const (
	// version 0x01000001 tells the device checksums may be skipped.
	protocolVersion = 0x01000001
	maxPayload      = 256 * 1024
	headerSize      = 24
)

type message struct {
	command    uint32
	arg0       uint32
	arg1       uint32
	dataLength uint32
	dataCheck  uint32
	magic      uint32
}

func (m *message) encode(buf []byte) {
	binary.LittleEndian.PutUint32(buf[0:], m.command)
	binary.LittleEndian.PutUint32(buf[4:], m.arg0)
	binary.LittleEndian.PutUint32(buf[8:], m.arg1)
	binary.LittleEndian.PutUint32(buf[12:], m.dataLength)
	binary.LittleEndian.PutUint32(buf[16:], m.dataCheck)
	binary.LittleEndian.PutUint32(buf[20:], m.magic)
}

func decodeMessage(buf []byte) message {
	return message{
		command:    binary.LittleEndian.Uint32(buf[0:]),
		arg0:       binary.LittleEndian.Uint32(buf[4:]),
		arg1:       binary.LittleEndian.Uint32(buf[8:]),
		dataLength: binary.LittleEndian.Uint32(buf[12:]),
		dataCheck:  binary.LittleEndian.Uint32(buf[16:]),
		magic:      binary.LittleEndian.Uint32(buf[20:]),
	}
}

// checksum is the additive sum the protocol calls data_crc32. It is not a
// CRC despite the field name, and devices announcing version 0x01000001 or
// later ignore it.
func checksum(data []byte) uint32 {
	var sum uint32
	for _, b := range data {
		sum += uint32(b)
	}
	return sum
}

type packet struct {
	msg  message
	data []byte
}

// Conn is an authenticated connection to one device, multiplexing streams over
// the single pair of bulk endpoints.
type Conn struct {
	dev     *winusb.Device
	Banner  string
	MaxData uint32

	// Serial is the device's ro.serialno, read once at connect time. It is
	// empty if the device did not report one.
	Serial string

	writeMu sync.Mutex

	mu      sync.Mutex
	streams map[uint32]*Stream
	nextID  uint32

	closeOnce sync.Once
	done      chan struct{}
	err       error
	wg        sync.WaitGroup
}

// Dial claims the first ADB interface on the machine and authenticates.
func Dial() (*Conn, error) { return DialTimeout(15*time.Second, nil) }

// DialTimeout waits for a device to become claimable.
//
// The wait is not defensive padding. When a host disconnects, adbd on the phone
// tears its USB function down and brings it back up, so the interface vanishes
// from enumeration entirely. Measured on a Pixel 7 it was gone for 3.6 seconds
// after a clean close, which is exactly what running this tool twice in a row
// hits. notify, if given, is called once when the wait starts.
//
// It requires exactly one connected device; see DialTimeoutSelect to name one
// among several.
func DialTimeout(timeout time.Duration, notify func()) (*Conn, error) {
	return DialTimeoutSelect(timeout, "", notify)
}

// DialPath claims one specific interface and authenticates.
func DialPath(path string) (*Conn, error) {
	dev, err := winusb.Open(path)
	if err != nil {
		return nil, err
	}
	c := &Conn{
		dev:     dev,
		MaxData: maxPayload,
		streams: map[uint32]*Stream{},
		nextID:  1,
		done:    make(chan struct{}),
	}
	if err := c.connect(DefaultAuthWait); err != nil {
		dev.Close()
		return nil, err
	}
	c.wg.Add(1)
	go c.readLoop()

	// The CNXN banner carries no serial, so it takes a round trip. It is read
	// here rather than on demand because the backup index needs it to notice a
	// folder being pointed at a different phone. A device that refuses getprop
	// is still perfectly usable for a backup, so only a device that has stopped
	// answering entirely is fatal: it would hang the first stream anyway.
	stop := cancelAfter(serialWait, c.Abort)
	out, err := c.Exec("getprop ro.serialno")
	stop()
	switch {
	case err == nil:
		c.Serial = strings.TrimSpace(string(out))
	case errors.Is(err, ErrAborted):
		c.Close()
		return nil, fmt.Errorf("the device stopped responding right after connecting: %w", err)
	}
	return c, nil
}

// send writes one message, splitting nothing: callers keep payloads within
// MaxData.
func (c *Conn) send(cmd, arg0, arg1 uint32, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	m := message{
		command:    cmd,
		arg0:       arg0,
		arg1:       arg1,
		dataLength: uint32(len(data)),
		dataCheck:  checksum(data),
		magic:      cmd ^ 0xffffffff,
	}
	var hdr [headerSize]byte
	m.encode(hdr[:])
	if err := c.dev.WriteAll(hdr[:]); err != nil {
		return err
	}
	if len(data) > 0 {
		return c.dev.WriteAll(data)
	}
	return nil
}

// recv reads one message and its payload.
func (c *Conn) recv() (packet, error) {
	var hdr [headerSize]byte
	if err := c.dev.ReadFull(hdr[:]); err != nil {
		return packet{}, err
	}
	m := decodeMessage(hdr[:])
	if m.magic != m.command^0xffffffff {
		return packet{}, fmt.Errorf("corrupt adb header: command %#x magic %#x", m.command, m.magic)
	}
	if m.dataLength == 0 {
		return packet{msg: m}, nil
	}
	if m.dataLength > 1<<24 {
		return packet{}, fmt.Errorf("implausible payload length %d", m.dataLength)
	}
	data := make([]byte, m.dataLength)
	if err := c.dev.ReadFull(data); err != nil {
		return packet{}, err
	}
	return packet{msg: m, data: data}, nil
}

// OnAuthPrompt is called once, just before the handshake starts waiting for the
// user to accept the "Allow USB debugging?" dialog on the phone.
//
// Without it the wait is indistinguishable from a hang: the tool sits silent
// for up to DefaultAuthWait while the answer is on a screen the user may not
// even be looking at.
var OnAuthPrompt func()

const (
	// DefaultAuthWait is how long DialPath waits for a human to tap "Allow USB
	// debugging?". It is generous because the phone may be face down.
	DefaultAuthWait = 60 * time.Second

	// handshakeWait bounds the part of the handshake the device answers on its
	// own. A phone that has stopped talking never answers at all, and recv has
	// no timeout of its own, so without this the very first read hangs forever.
	handshakeWait = 10 * time.Second

	// serialWait bounds the one getprop DialPath runs, for the same reason.
	serialWait = 10 * time.Second
)

// cancelAfter arms fn to run once after d, unless the returned stop is called
// first.
//
// time.Timer.Stop alone is not enough here: it does not wait for a timer that
// has already begun running, and every fn passed here tears a connection down,
// so one landing a moment too late would kill a perfectly healthy connection.
func cancelAfter(d time.Duration, fn func()) (stop func()) {
	var mu sync.Mutex
	done := false
	t := time.AfterFunc(d, func() {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			fn()
		}
	})
	return func() {
		t.Stop()
		mu.Lock()
		done = true
		mu.Unlock()
	}
}

// errNotAuthorized is the standing failure when the user never accepts.
var errNotAuthorized = errors.New("this computer is not authorized on the device yet: " +
	"accept the \"Allow USB debugging?\" prompt on the phone, then run again")

// connect performs the CNXN/AUTH handshake, waiting up to authWait for the user
// to authorize this computer if the device does not already know the key.
//
// Signing is per token, not per connection. adbd sends a fresh AUTH TOKEN after
// the user accepts, and the old code had already given up by then: it offered
// the public key and returned an error, so the first run always failed and only
// a second one connected. Real adb keeps signing until CNXN arrives.
func (c *Conn) connect(authWait time.Duration) error {
	banner := "host::features=shell_v2,cmd,stat_v2,ls_v2,sendrecv_v2\x00"
	if err := c.send(cmdCNXN, protocolVersion, maxPayload, []byte(banner)); err != nil {
		return fmt.Errorf("sending CNXN: %w", err)
	}

	key, keyErr := loadPrivateKey()
	signed := 0
	sentPublicKey := false
	var authDeadline time.Time

	// recv parks in a USB read that nothing else can interrupt, so the wait is
	// bounded by cancelling the transfer out from under it. A device unplugged
	// while the dialog is up therefore fails instead of hanging the whole tool.
	stopWatchdog := cancelAfter(handshakeWait, c.dev.CancelIO)
	defer func() { stopWatchdog() }()

	for {
		p, err := c.recv()
		if err != nil {
			if !authDeadline.IsZero() && !time.Now().Before(authDeadline) {
				return errNotAuthorized
			}
			return fmt.Errorf("during handshake: %w", err)
		}
		switch p.msg.command {
		case cmdCNXN:
			c.Banner = strings.TrimRight(string(p.data), "\x00")
			if p.msg.arg1 > 0 && p.msg.arg1 < c.MaxData {
				c.MaxData = p.msg.arg1
			}
			return nil

		case cmdAUTH:
			if p.msg.arg0 != authToken {
				return fmt.Errorf("unexpected AUTH type %d", p.msg.arg0)
			}
			if keyErr != nil {
				return fmt.Errorf("device asked for authentication but no key is usable: %w", keyErr)
			}
			// The token is already a SHA-1 digest, so it is signed with
			// PKCS#1 v1.5 using the SHA-1 DigestInfo prefix.
			sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA1, p.data)
			if err != nil {
				return fmt.Errorf("signing auth token: %w", err)
			}
			if err := c.send(cmdAUTH, authSignature, 0, sig); err != nil {
				return err
			}
			signed++

			// A second token means the signature was rejected: the device does
			// not know this key, so offer the public half. That is what raises
			// the prompt, and everything after it is waiting on a human.
			if signed > 1 && !sentPublicKey {
				pub, err := loadPublicKey()
				if err != nil {
					return fmt.Errorf("device rejected the key and no public key is available: %w", err)
				}
				if err := c.send(cmdAUTH, authRSAPublicKey, 0, pub); err != nil {
					return err
				}
				sentPublicKey = true
				authDeadline = time.Now().Add(authWait)
				stopWatchdog()
				stopWatchdog = cancelAfter(authWait, c.dev.CancelIO)
				if OnAuthPrompt != nil {
					OnAuthPrompt()
				}
			}
			if sentPublicKey && !time.Now().Before(authDeadline) {
				return errNotAuthorized
			}

		default:
			// Ignore anything else until the connection settles.
		}
	}
}

// readLoop dispatches incoming packets to their streams.
func (c *Conn) readLoop() {
	defer c.wg.Done()
	for {
		p, err := c.recv()
		if err != nil {
			c.fail(err)
			return
		}
		switch p.msg.command {
		case cmdOKAY:
			if s := c.stream(p.msg.arg1); s != nil {
				s.setRemote(p.msg.arg0)
				select {
				case s.ready <- struct{}{}:
				default:
				}
			}
		case cmdWRTE:
			s := c.stream(p.msg.arg1)
			if s == nil {
				continue
			}
			select {
			case s.in <- p.data:
			case <-s.closed:
				continue
			case <-c.done:
				return
			}
			// The sender waits for this before sending more.
			if err := c.send(cmdOKAY, s.local, s.remoteID(), nil); err != nil {
				c.fail(err)
				return
			}
		case cmdCLSE:
			if s := c.stream(p.msg.arg1); s != nil {
				s.shutdown()
			}
		}
	}
}

func (c *Conn) stream(id uint32) *Stream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams[id]
}

func (c *Conn) fail(err error) {
	c.closeOnce.Do(func() {
		c.err = err
		close(c.done)
		c.mu.Lock()
		for _, s := range c.streams {
			s.shutdown()
		}
		c.mu.Unlock()
	})
}

// Open starts a stream for a service such as "sync:" or "shell:ls".
func (c *Conn) Open(service string) (*Stream, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	s := &Stream{
		conn:   c,
		local:  id,
		in:     make(chan []byte, 8),
		ready:  make(chan struct{}, 1),
		closed: make(chan struct{}),
	}
	c.streams[id] = s
	c.mu.Unlock()

	if err := c.send(cmdOPEN, id, 0, append([]byte(service), 0)); err != nil {
		c.removeStream(id)
		return nil, err
	}
	select {
	case <-s.ready:
		return s, nil
	case <-s.closed:
		c.removeStream(id)
		return nil, fmt.Errorf("device refused service %q", service)
	case <-c.done:
		return nil, c.err
	}
}

func (c *Conn) removeStream(id uint32) {
	c.mu.Lock()
	delete(c.streams, id)
	c.mu.Unlock()
}

// ErrAborted reports that the connection was torn down by Abort.
var ErrAborted = errors.New("the USB connection was aborted")

// Abort tears the connection down without waiting for the reader to return.
//
// Close waits for readLoop, which is parked in a USB read; if the device has
// stopped responding that wait never ends. Abort cancels the in-flight transfer
// and marks the connection failed, so every goroutine blocked on a stream
// unblocks immediately. It is safe to call more than once, and safe to call
// concurrently with Close.
func (c *Conn) Abort() {
	// No dev.Close and no wg.Wait here: freeing the handles is Close's job, and
	// Close must still work afterwards. All this does is make every blocked
	// goroutine observable, so Ctrl+C and the GUI's stop button have an effect.
	c.fail(fmt.Errorf("%w: any transfer still in flight was cut short", ErrAborted))
	c.dev.CancelIO()
}

// Close shuts the connection down in the order the driver requires: stop the
// reader first, then release the interface.
func (c *Conn) Close() error {
	c.fail(errors.New("connection closed"))
	// Unblock readLoop, which is otherwise parked in a USB read, and wait for
	// it to return before any handle is freed.
	c.dev.CancelIO()
	c.wg.Wait()
	return c.dev.Close()
}

// Stream is one multiplexed channel to a device service.
type Stream struct {
	conn   *Conn
	local  uint32
	in     chan []byte
	ready  chan struct{}
	closed chan struct{}

	mu     sync.Mutex
	remote uint32
	buf    []byte
	once   sync.Once
}

func (s *Stream) setRemote(id uint32) {
	s.mu.Lock()
	if s.remote == 0 {
		s.remote = id
	}
	s.mu.Unlock()
}

func (s *Stream) remoteID() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remote
}

func (s *Stream) shutdown() { s.once.Do(func() { close(s.closed) }) }

// Read returns bytes from the device, blocking until some arrive.
func (s *Stream) Read(p []byte) (int, error) {
	if len(s.buf) == 0 {
		select {
		case b := <-s.in:
			s.buf = b
		case <-s.closed:
			// Drain anything already delivered before reporting EOF.
			select {
			case b := <-s.in:
				s.buf = b
			default:
				return 0, io.EOF
			}
		case <-s.conn.done:
			return 0, s.conn.err
		}
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

// ReadFull reads exactly len(p) bytes.
func (s *Stream) ReadFull(p []byte) error {
	_, err := io.ReadFull(s, p)
	return err
}

// Write sends data to the device, respecting the protocol's one-in-flight
// acknowledgement rule and the negotiated payload limit.
func (s *Stream) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := len(p)
		if uint32(n) > s.conn.MaxData {
			n = int(s.conn.MaxData)
		}
		if err := s.conn.send(cmdWRTE, s.local, s.remoteID(), p[:n]); err != nil {
			return total, err
		}
		select {
		case <-s.ready:
		case <-s.closed:
			return total, io.ErrClosedPipe
		case <-s.conn.done:
			return total, s.conn.err
		}
		p = p[n:]
		total += n
	}
	return total, nil
}

// Close tears down the stream.
func (s *Stream) Close() error {
	s.conn.send(cmdCLSE, s.local, s.remoteID(), nil)
	s.shutdown()
	s.conn.removeStream(s.local)
	return nil
}

// loadPrivateKey reads the same key adb uses, so a device that has already
// authorized this computer stays authorized, and creates one if there is none.
//
// Generating is what makes "no adb.exe required" true: on a computer that has
// never run adb, ~/.android/adbkey does not exist and the handshake used to die
// with "no key is usable: ... no such file", which no amount of retrying fixed.
func loadPrivateKey() (*rsa.PrivateKey, error) {
	path, err := keyPath("adbkey")
	if err != nil {
		return nil, err
	}
	key, err := readPrivateKey(path)
	if errors.Is(err, fs.ErrNotExist) && keysAreOurs() {
		return generatePrivateKey(path)
	}
	return key, err
}

func readPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s is not PEM", path)
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%s is not an RSA key", path)
		}
		return rk, nil
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// loadPublicKey returns adbkey.pub verbatim. It is already in Android's own
// public key encoding, so there is nothing to convert.
//
// If only the public half is missing it is derived from the private key rather
// than treated as fatal, because the pair has to be complete before the device
// can be asked to trust it.
func loadPublicKey() ([]byte, error) {
	path, err := keyPath("adbkey.pub")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && keysAreOurs() {
		data, err = derivePublicKey(path)
	}
	if err != nil {
		return nil, err
	}
	// adbd expects the blob NUL-terminated, and the trailing newline adb writes
	// into the file is not part of it.
	return append([]byte(strings.TrimRight(string(data), "\r\n ")), 0), nil
}

// derivePublicKey writes adbkey.pub next to an existing (or freshly generated)
// private key and returns its contents. The exponent is whatever the private
// key actually has, so a key made elsewhere is described truthfully.
func derivePublicKey(path string) ([]byte, error) {
	key, err := loadPrivateKey()
	if err != nil {
		return nil, err
	}
	data, err := encodePublicKeyFile(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	if _, err := writeNew(path, data, 0o644); err != nil {
		return nil, err
	}
	return data, nil
}

// keysAreOurs reports whether the key directory is ours to write into.
//
// ANDROID_VENDOR_KEYS names a directory the caller curates, often read-only or
// shared, so a missing key there is a configuration error to report rather than
// something to paper over by inventing a new identity.
func keysAreOurs() bool { return os.Getenv("ANDROID_VENDOR_KEYS") == "" }

func keyPath(name string) (string, error) {
	if v := os.Getenv("ANDROID_VENDOR_KEYS"); v != "" {
		return filepath.Join(v, name), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".android", name), nil
}
