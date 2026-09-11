package adbproto

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// testKey is shared because generating 2048-bit RSA keys is the slow part of
// these tests and nothing here mutates the key.
var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
)

func sharedTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, adbKeyBits)
		if err != nil {
			t.Fatalf("generating a test key: %v", err)
		}
		testKey = k
	})
	return testKey
}

// TestAndroidPublicKeyEncoding checks the blob against the definition adbd
// parses, field by field. A wrong n0inv or rr is not something the device
// reports: it just never recognises the key.
func TestAndroidPublicKeyEncoding(t *testing.T) {
	key := sharedTestKey(t)
	blob, err := androidPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("androidPublicKey: %v", err)
	}
	if len(blob) != 524 {
		t.Fatalf("blob is %d bytes, want 524", len(blob))
	}

	if words := binary.LittleEndian.Uint32(blob[0:]); words != 64 {
		t.Errorf("modulus_size_words = %d, want 64", words)
	}
	if e := binary.LittleEndian.Uint32(blob[520:]); e != 65537 {
		t.Errorf("exponent = %d, want 65537", e)
	}

	// The modulus is the key's N written least significant byte first.
	if got := beFromLE(blob[8:264]); got.Cmp(key.N) != 0 {
		t.Errorf("modulus does not round-trip back to N")
	}

	// n0inv is -1/n mod 2^32, so n0inv*n0 + 1 must vanish mod 2^32.
	n0inv := uint64(binary.LittleEndian.Uint32(blob[4:]))
	n0 := new(big.Int).And(key.N, big.NewInt(0xffffffff)).Uint64()
	if (n0inv*n0+1)&0xffffffff != 0 {
		t.Errorf("n0inv %d is not the inverse of n0 %d mod 2^32", n0inv, n0)
	}

	// rr is (2^2048)^2 mod n.
	r := new(big.Int).Lsh(big.NewInt(1), 2048)
	wantRR := new(big.Int).Mod(new(big.Int).Mul(r, r), key.N)
	if got := beFromLE(blob[264:520]); got.Cmp(wantRR) != 0 {
		t.Errorf("rr is not (2^2048)^2 mod n")
	}
}

// beFromLE reads a little-endian magnitude back as a big.Int.
func beFromLE(le []byte) *big.Int {
	be := make([]byte, len(le))
	for i, b := range le {
		be[len(le)-1-i] = b
	}
	return new(big.Int).SetBytes(be)
}

func TestPublicKeyFileFormat(t *testing.T) {
	key := sharedTestKey(t)
	data, err := encodePublicKeyFile(&key.PublicKey)
	if err != nil {
		t.Fatalf("encodePublicKeyFile: %v", err)
	}
	line := string(data)
	if !strings.HasSuffix(line, "\n") {
		t.Errorf("adbkey.pub should end with a newline, got %q", line)
	}
	fields := strings.Fields(line)
	if len(fields) != 2 {
		t.Fatalf("want '<base64> <comment>', got %q", line)
	}
	blob, err := base64.StdEncoding.DecodeString(fields[0])
	if err != nil {
		t.Fatalf("first field is not standard base64: %v", err)
	}
	if len(blob) != 524 {
		t.Errorf("decoded blob is %d bytes, want 524", len(blob))
	}
	if !strings.Contains(fields[1], "@") {
		t.Errorf("comment %q is not user@host", fields[1])
	}
}

// TestGenerateThenLoad is the first-run path: a home directory that has never
// seen adb must end up with a usable, stable pair rather than the old
// "no key is usable: ... no such file".
func TestGenerateThenLoad(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ANDROID_VENDOR_KEYS", "")
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	var generated []string
	OnKeyGenerated = func(p string) { generated = append(generated, p) }
	t.Cleanup(func() { OnKeyGenerated = nil })

	key, err := loadPrivateKey()
	if err != nil {
		t.Fatalf("loadPrivateKey on a fresh home: %v", err)
	}
	if key.N.BitLen() != adbKeyBits {
		t.Errorf("generated a %d-bit key, want %d", key.N.BitLen(), adbKeyBits)
	}
	if len(generated) != 1 {
		t.Fatalf("OnKeyGenerated fired %d times, want 1", len(generated))
	}
	if want := filepath.Join(home, ".android", "adbkey"); generated[0] != want {
		t.Errorf("generated %q, want %q", generated[0], want)
	}

	pub, err := loadPublicKey()
	if err != nil {
		t.Fatalf("loadPublicKey: %v", err)
	}
	if pub[len(pub)-1] != 0 {
		t.Errorf("the blob handed to adbd must be NUL-terminated")
	}
	blob, err := base64.StdEncoding.DecodeString(strings.Fields(string(pub))[0])
	if err != nil {
		t.Fatalf("adbkey.pub is not base64: %v", err)
	}
	if got := beFromLE(blob[8:264]); got.Cmp(key.N) != 0 {
		t.Errorf("adbkey.pub describes a different key than adbkey")
	}
	if _, err := os.Stat(filepath.Join(home, ".android", "adbkey.pub")); err != nil {
		t.Errorf("adbkey.pub was not written: %v", err)
	}

	// A second run must reuse the pair: regenerating would silently
	// de-authorize this computer on every device that already trusts it.
	again, err := loadPrivateKey()
	if err != nil {
		t.Fatalf("second loadPrivateKey: %v", err)
	}
	if again.N.Cmp(key.N) != 0 {
		t.Errorf("the key changed between runs")
	}
	if len(generated) != 1 {
		t.Errorf("the key was generated again on the second load")
	}
}

// TestVendorKeysAreNeverGenerated: ANDROID_VENDOR_KEYS names a directory the
// caller curates, so a missing key there is a configuration error to report,
// not a reason to invent a new identity.
func TestVendorKeysAreNeverGenerated(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ANDROID_VENDOR_KEYS", dir)

	if _, err := loadPrivateKey(); err == nil {
		t.Errorf("loadPrivateKey created a key in a caller-supplied directory")
	}
	if _, err := loadPublicKey(); err == nil {
		t.Errorf("loadPublicKey created a key in a caller-supplied directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%d file(s) written into the vendor key directory", len(entries))
	}
}
