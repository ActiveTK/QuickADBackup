package adbproto

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAndroidPublicKeyMatchesAdbsOwn is the only check that proves the encoding
// is right rather than merely self-consistent: it re-derives the public key from
// the private half of a pair that adb itself wrote, and compares the result with
// the adbkey.pub sitting next to it byte for byte.
//
// It skips when the machine has never run adb, since there is then nothing
// authoritative to compare against.
func TestAndroidPublicKeyMatchesAdbsOwn(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	privPEM, err := os.ReadFile(filepath.Join(home, ".android", "adbkey"))
	if err != nil {
		t.Skip("no adbkey written by adb to compare against")
	}
	pubFile, err := os.ReadFile(filepath.Join(home, ".android", "adbkey.pub"))
	if err != nil {
		t.Skip("no adbkey.pub written by adb to compare against")
	}

	block, _ := pem.Decode(privPEM)
	if block == nil {
		t.Fatalf("adbkey is not PEM")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		key = k.(*rsa.PrivateKey)
	} else if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else {
		t.Fatalf("cannot parse adbkey: %v", err)
	}

	got, err := androidPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("androidPublicKey: %v", err)
	}
	// adb writes "<base64> <comment>"; only the key material is ours to match.
	want := strings.Fields(strings.TrimSpace(string(pubFile)))[0]
	if gotB64 := base64.StdEncoding.EncodeToString(got); gotB64 != want {
		t.Errorf("encoded public key does not match the one adb wrote\n got: %s\nwant: %s",
			gotB64, want)
	}
}
