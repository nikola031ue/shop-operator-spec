/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package wallet

import (
	"errors"
	"strings"
	"testing"
)

// EIP-55 reference vectors (https://eips.ethereum.org/EIPS/eip-55).
var eip55 = []string{
	"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
	"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
	"0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB",
	"0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb",
}

func TestNormalizeAddress(t *testing.T) {
	for _, want := range eip55 {
		t.Run(want, func(t *testing.T) {
			for name, in := range map[string]string{
				"checksummed": want,
				"lowercase":   "0x" + strings.ToLower(want[2:]),
				"uppercase":   "0x" + strings.ToUpper(want[2:]),
			} {
				got, err := NormalizeAddress(in)
				if err != nil {
					t.Fatalf("%s %q: unexpected error %v", name, in, err)
				}
				if got != want {
					t.Fatalf("%s %q: got %q, want %q", name, in, got, want)
				}
			}
		})
	}
}

func TestNormalizeAddressBadChecksum(t *testing.T) {
	for _, good := range eip55 {
		// Flip the case of the first letter in the hex part: still mixed case, wrong checksum.
		b := []byte(good)
		for i := 2; i < len(b); i++ {
			if c := b[i]; c >= 'a' && c <= 'f' {
				b[i] = c - 'a' + 'A'
				break
			} else if c >= 'A' && c <= 'F' {
				b[i] = c - 'A' + 'a'
				break
			}
		}
		if _, err := NormalizeAddress(string(b)); !errors.Is(err, ErrBadChecksum) {
			t.Fatalf("%q: want ErrBadChecksum, got %v", b, err)
		}
	}
}

func TestNormalizeAddressInvalid(t *testing.T) {
	for _, in := range []string{"", "0x123", "5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAeZ", "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed00"} {
		if _, err := NormalizeAddress(in); !errors.Is(err, ErrInvalidAddress) {
			t.Fatalf("%q: want ErrInvalidAddress, got %v", in, err)
		}
	}
}

func TestGenerateKeyRoundTrip(t *testing.T) {
	priv, addr, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(priv) != 64 || strings.HasPrefix(priv, "0x") {
		t.Fatalf("private key must be 64 hex chars without 0x, got length %d", len(priv))
	}
	if got, err := NormalizeAddress(addr); err != nil || got != addr {
		t.Fatalf("generated address %q is not checksummed (got %q, %v)", addr, got, err)
	}
	for _, in := range []string{priv, "0x" + priv, " " + priv + "\n"} {
		derived, err := AddressFromPrivateKeyHex(in)
		if err != nil || derived != addr {
			t.Fatalf("derive: got %q, %v; want %q", derived, err, addr)
		}
	}
	priv2, addr2, err := GenerateKey()
	if err != nil || priv2 == priv || addr2 == addr {
		t.Fatal("two generated keys must differ")
	}
}

func TestAddressFromPrivateKeyHexInvalidHidesKey(t *testing.T) {
	secret := "not-a-key-but-must-not-leak"
	_, err := AddressFromPrivateKeyHex(secret)
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("want ErrInvalidKey, got %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("error message must not contain the key material")
	}
}
