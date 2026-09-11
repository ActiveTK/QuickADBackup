package adbproto

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
)

// OnKeyGenerated is called once when a fresh adbkey had to be created, with the
// path it was written to.
//
// It exists so the caller can warn the user that the phone is about to show an
// "Allow USB debugging?" prompt: on a computer that has never run adb there is
// no key to recognise, so the very first connection always needs a human.
var OnKeyGenerated func(path string)

// adbKeyBits is the only key size adb's own public key encoding can express:
// the struct below has a fixed 256-byte modulus field.
const adbKeyBits = 2048

// adbModulusBytes and adbModulusWords describe that fixed field.
const (
	adbModulusBytes = adbKeyBits / 8
	adbModulusWords = adbModulusBytes / 4
)

// generatePrivateKey creates the key adb would have created and writes it where
// adb would look for it, so the two stay interchangeable: a device authorized
// through this tool is authorized for adb.exe as well, and the other way round.
func generatePrivateKey(path string) (*rsa.PrivateKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, adbKeyBits)
	if err != nil {
		return nil, fmt.Errorf("generating an adb key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	written, err := writeNew(path, pemBytes, 0o600)
	if err != nil {
		return nil, err
	}
	if !written {
		// Another process created the key while this one was generating. Theirs
		// is the one the device will be told about, so use it.
		return readPrivateKey(path)
	}
	if OnKeyGenerated != nil {
		OnKeyGenerated(path)
	}
	return key, nil
}

// writeNew writes data to path through a temporary file in the same directory,
// and reports whether it won the race. An existing key is never replaced:
// overwriting it would silently de-authorize this computer on every device that
// already trusts it.
func writeNew(path string, data []byte, mode os.FileMode) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	// CreateTemp already uses 0600; this states the intent, and matters on the
	// platforms where adb refuses a world-readable key.
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return false, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return false, err
	}
	// The key has to survive the crash that interrupts its first use, otherwise
	// the device is left trusting a key this computer no longer has.
	if err := f.Sync(); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, err
	}
	return true, nil
}

// androidPublicKey encodes a public key the way adb does.
//
// The format is neither PEM nor SubjectPublicKeyInfo but the fixed 524-byte
// RSAPublicKey struct from AOSP's crypto/rsa_key.c, with every integer
// little-endian:
//
//	uint32 modulus_size_words  // 64
//	uint32 n0inv               // -1/n mod 2^32
//	uint8  modulus[256]        // n, least significant byte first
//	uint8  rr[256]             // (2^2048)^2 mod n, same order
//	uint32 exponent
//
// n0inv and rr are the Montgomery constants the bootloader-era verifier needs;
// adbd stores them verbatim and compares the whole blob, so they have to be
// right even though nothing here uses them.
func androidPublicKey(pub *rsa.PublicKey) ([]byte, error) {
	n := pub.N
	if n.BitLen() > adbKeyBits {
		return nil, fmt.Errorf("adb keys must be %d-bit, this one is %d-bit", adbKeyBits, n.BitLen())
	}

	n0 := new(big.Int).And(n, big.NewInt(0xffffffff))
	mod32 := new(big.Int).Lsh(big.NewInt(1), 32)
	inv := new(big.Int).ModInverse(n0, mod32)
	if inv == nil {
		return nil, fmt.Errorf("modulus has no inverse mod 2^32, which means it is even")
	}
	n0inv := uint32(1<<32 - inv.Uint64())

	r := new(big.Int).Lsh(big.NewInt(1), 32*adbModulusWords)
	rr := new(big.Int).Mod(new(big.Int).Mul(r, r), n)

	out := make([]byte, 8+2*adbModulusBytes+4)
	binary.LittleEndian.PutUint32(out[0:], adbModulusWords)
	binary.LittleEndian.PutUint32(out[4:], n0inv)
	if err := putLittleEndian(out[8:8+adbModulusBytes], n); err != nil {
		return nil, err
	}
	if err := putLittleEndian(out[8+adbModulusBytes:8+2*adbModulusBytes], rr); err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint32(out[8+2*adbModulusBytes:], uint32(pub.E))
	return out, nil
}

// putLittleEndian writes v into dst least significant byte first, zero-padded
// to the full width of dst. big.Int hands out big-endian magnitudes with no
// padding, so both steps are needed.
func putLittleEndian(dst []byte, v *big.Int) error {
	be := v.Bytes()
	if len(be) > len(dst) {
		return fmt.Errorf("value needs %d bytes but the field holds %d", len(be), len(dst))
	}
	for i, b := range be {
		dst[len(be)-1-i] = b
	}
	for i := len(be); i < len(dst); i++ {
		dst[i] = 0
	}
	return nil
}

// encodePublicKeyFile renders the contents of adbkey.pub: the encoded key in
// standard base64, a space, and a user@host comment.
func encodePublicKeyFile(pub *rsa.PublicKey) ([]byte, error) {
	blob, err := androidPublicKey(pub)
	if err != nil {
		return nil, err
	}
	line := base64.StdEncoding.EncodeToString(blob) + " " + keyComment() + "\n"
	return []byte(line), nil
}

// keyComment names the machine the key belongs to, the way adb does. It is
// decoration: the device shows it in the authorization dialog and in Settings.
func keyComment() string {
	user := ""
	if home, err := os.UserHomeDir(); err == nil {
		user = filepath.Base(home)
	}
	host, err := os.Hostname()
	if err != nil || user == "" || user == "." || host == "" {
		return "quickadbackup@localhost"
	}
	return user + "@" + host
}
