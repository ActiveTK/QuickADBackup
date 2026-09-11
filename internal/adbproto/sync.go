package adbproto

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Sync service request and response ids, as documented in SYNC.TXT.
var (
	idSTAT2 = [4]byte{'S', 'T', 'A', '2'}
	idLIST  = [4]byte{'L', 'I', 'S', 'T'}
	idLIST2 = [4]byte{'L', 'I', 'S', '2'}
	idRECV  = [4]byte{'R', 'E', 'C', 'V'}
	idDENT  = [4]byte{'D', 'E', 'N', 'T'}
	idDENT2 = [4]byte{'D', 'N', 'T', '2'}
	idDONE  = [4]byte{'D', 'O', 'N', 'E'}
	idDATA  = [4]byte{'D', 'A', 'T', 'A'}
	idFAIL  = [4]byte{'F', 'A', 'I', 'L'}
	idQUIT  = [4]byte{'Q', 'U', 'I', 'T'}
)

// SyncConn is one sync-service stream.
//
// Only STAT, LIST and RECV are implemented. SEND is deliberately absent: this
// tool must never write to the device.
type SyncConn struct {
	s *Stream
}

// Sync starts a sync-service stream. Several may run at once on one Conn; the
// protocol multiplexes them.
func (c *Conn) Sync() (*SyncConn, error) {
	s, err := c.Open("sync:")
	if err != nil {
		return nil, err
	}
	return &SyncConn{s: s}, nil
}

func (sc *SyncConn) Close() error {
	sc.request(idQUIT, nil)
	return sc.s.Close()
}

func (sc *SyncConn) request(id [4]byte, payload []byte) error {
	hdr := make([]byte, 8, 8+len(payload))
	copy(hdr, id[:])
	binary.LittleEndian.PutUint32(hdr[4:], uint32(len(payload)))
	_, err := sc.s.Write(append(hdr, payload...))
	return err
}

func (sc *SyncConn) readID() ([4]byte, error) {
	var b [4]byte
	if err := sc.s.ReadFull(b[:]); err != nil {
		return b, err
	}
	return b, nil
}

func (sc *SyncConn) readU32() (uint32, error) {
	var b [4]byte
	if err := sc.s.ReadFull(b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}

// failMessage reads the message body of a FAIL response.
func (sc *SyncConn) failMessage() error {
	n, err := sc.readU32()
	if err != nil {
		return err
	}
	msg := make([]byte, n)
	if err := sc.s.ReadFull(msg); err != nil {
		return err
	}
	return fmt.Errorf("device refused: %s", msg)
}

// FileInfo is one entry's metadata, with the 64-bit fields of the v2 protocol.
type FileInfo struct {
	Name  string
	Mode  uint32
	Size  int64
	MTime int64 // unix seconds
	Err   uint32
}

func (f FileInfo) IsDir() bool     { return f.Mode&0o170000 == 0o040000 }
func (f FileInfo) IsRegular() bool { return f.Mode&0o170000 == 0o100000 }

// ModTime is the modification time as a Go value.
func (f FileInfo) ModTime() time.Time { return time.Unix(f.MTime, 0) }

// statV2Size is the wire size of the v2 metadata block that follows the id.
const statV2Size = 68

func parseStatV2(b []byte) FileInfo {
	return FileInfo{
		Err:   binary.LittleEndian.Uint32(b[0:]),
		Mode:  binary.LittleEndian.Uint32(b[20:]),
		Size:  int64(binary.LittleEndian.Uint64(b[36:])),
		MTime: int64(binary.LittleEndian.Uint64(b[52:])),
	}
}

// Stat returns metadata for one path.
func (sc *SyncConn) Stat(path string) (FileInfo, error) {
	if err := sc.request(idSTAT2, []byte(path)); err != nil {
		return FileInfo{}, err
	}
	id, err := sc.readID()
	if err != nil {
		return FileInfo{}, err
	}
	if id == idFAIL {
		return FileInfo{}, sc.failMessage()
	}
	if id != idSTAT2 {
		return FileInfo{}, fmt.Errorf("unexpected sync response %q to STAT", id)
	}
	buf := make([]byte, statV2Size)
	if err := sc.s.ReadFull(buf); err != nil {
		return FileInfo{}, err
	}
	fi := parseStatV2(buf)
	if fi.Err != 0 {
		return fi, fmt.Errorf("stat %s: device errno %d", path, fi.Err)
	}
	return fi, nil
}

// List returns the entries of one directory.
func (sc *SyncConn) List(path string) ([]FileInfo, error) {
	if err := sc.request(idLIST2, []byte(path)); err != nil {
		return nil, err
	}
	var out []FileInfo
	for {
		id, err := sc.readID()
		if err != nil {
			return nil, err
		}
		switch id {
		case idDONE:
			// DONE carries a zeroed entry body.
			if err := sc.s.ReadFull(make([]byte, statV2Size+4)); err != nil {
				return nil, err
			}
			return out, nil
		case idFAIL:
			return nil, sc.failMessage()
		case idDENT2:
			buf := make([]byte, statV2Size+4)
			if err := sc.s.ReadFull(buf); err != nil {
				return nil, err
			}
			fi := parseStatV2(buf)
			nameLen := binary.LittleEndian.Uint32(buf[statV2Size:])
			name := make([]byte, nameLen)
			if err := sc.s.ReadFull(name); err != nil {
				return nil, err
			}
			fi.Name = string(name)
			out = append(out, fi)
		default:
			return nil, fmt.Errorf("unexpected sync response %q to LIST", id)
		}
	}
}

// Recv streams one file's contents to w.
//
// Unlike a batched tar, an unreadable file produces an explicit FAIL for that
// path rather than being silently omitted.
func (sc *SyncConn) Recv(path string, w io.Writer) (int64, error) {
	if err := sc.request(idRECV, []byte(path)); err != nil {
		return 0, err
	}
	var total int64
	for {
		id, err := sc.readID()
		if err != nil {
			return total, err
		}
		switch id {
		case idDATA:
			n, err := sc.readU32()
			if err != nil {
				return total, err
			}
			if _, err := io.CopyN(w, sc.s, int64(n)); err != nil {
				return total, err
			}
			total += int64(n)
		case idDONE:
			// DONE is followed by a single unused word.
			if _, err := sc.readU32(); err != nil {
				return total, err
			}
			return total, nil
		case idFAIL:
			return total, sc.failMessage()
		default:
			return total, fmt.Errorf("unexpected sync response %q to RECV", id)
		}
	}
}

// RecvFile writes one device file to a local path, preserving its mtime, and
// reports how many bytes actually arrived.
//
// The count is the transferred length rather than the length the directory
// listing claimed, because the two disagree whenever a file is still being
// written on the phone; the index has to record what is really on disk here.
func (sc *SyncConn) RecvFile(remote, local string, mtime time.Time) (int64, error) {
	tmp := local + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := sc.Recv(remote, f)
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return 0, err
	}
	// Flush to the platter before the rename makes the file look complete.
	// Without this a power loss during a backup leaves a full-length directory
	// entry whose contents are zeroes, and the index believes that file is done.
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return 0, err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, local); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if !mtime.IsZero() {
		os.Chtimes(local, mtime, mtime)
	}
	return n, nil
}

// ShellQuote wraps a string as a single-quoted token for the device's sh.
//
// An embedded apostrophe is escaped by closing the quoted run, emitting a
// backslash-escaped apostrophe, then reopening it. Tripling the apostrophe
// instead, as this used to, leaves the string unterminated: a filename with an
// apostrophe in it made the device answer "sh: unexpected EOF while looking for
// matching quote" and the command never ran at all.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ExecResult is the outcome of one device command.
type ExecResult struct {
	Out  []byte // the command's stdout, exactly as produced
	Code int    // the command's exit status
}

// execMarker introduces the exit status appended by ExecStatus. It is long and
// unlikely enough that a command's own output will not end with it, and the
// last occurrence wins so that even output which quotes it stays unambiguous.
const execMarker = "__QAB_EXIT:"

// splitExecMarker separates a command's stdout from the trailing exit marker.
//
// It is a pure function so the marker handling can be tested without a phone,
// which is how the ShellQuote bug stayed invisible for so long: exec: carries
// no exit status, so a command that did not even parse looked like success with
// empty output.
func splitExecMarker(out []byte) (stdout []byte, code int, ok bool) {
	i := bytes.LastIndex(out, []byte(execMarker))
	if i < 0 {
		return nil, 0, false
	}
	digits := out[i+len(execMarker):]
	end := 0
	for end < len(digits) && digits[end] >= '0' && digits[end] <= '9' {
		end++
	}
	if end == 0 {
		return nil, 0, false
	}
	code, err := strconv.Atoi(string(digits[:end]))
	if err != nil {
		return nil, 0, false
	}
	return out[:i], code, true
}

// ExecStatus runs a command on the device and reports both its output and its
// exit status.
//
// The exec service carries no exit status of its own, so the command is wrapped
// in a compound that prints one. The compound is closed by a newline before the
// brace so that a command ending in a comment or a redirect still parses.
func (c *Conn) ExecStatus(command string) (ExecResult, error) {
	service := "exec:{ " + command + "\n}\nprintf '" + execMarker + "%s\\n' \"$?\""
	s, err := c.Open(service)
	if err != nil {
		return ExecResult{}, err
	}
	defer s.Close()
	out, err := io.ReadAll(s)
	if err != nil {
		return ExecResult{}, err
	}
	stdout, code, ok := splitExecMarker(out)
	if !ok {
		head := out
		if len(head) > 200 {
			head = head[:200]
		}
		return ExecResult{}, fmt.Errorf("the device's shell did not run %q to completion: "+
			"no %s marker in its output %q", command, execMarker, head)
	}
	return ExecResult{Out: stdout, Code: code}, nil
}

// Exec runs a command and returns its stdout. The error is non-nil only when
// the device's shell could not run the command at all; a non-zero exit status is
// not an error here, because several callers run commands that legitimately fail
// on part of their input. Use ExecStatus for that.
//
// It uses the "exec" service rather than "shell", because shell allocates a
// pseudo-terminal that rewrites LF as CRLF and would corrupt any binary or
// newline-sensitive output.
func (c *Conn) Exec(command string) ([]byte, error) {
	r, err := c.ExecStatus(command)
	if err != nil {
		return nil, err
	}
	return r.Out, nil
}
