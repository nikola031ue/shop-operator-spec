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

// Package wallet holds the offline Ethereum account logic of the Wallet resource: key
// generation, address derivation and EIP-55 checksum validation (research R10). It never talks
// to a blockchain node (FR-011).
package wallet

import (
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"
)

// ErrInvalidKey is returned for a private key that is not 32 bytes of hex.
var ErrInvalidKey = errors.New("invalid private key")

// GenerateKey creates a new secp256k1 key. It returns the private key as lowercase hex
// without 0x prefix (for the key Secret) and the EIP-55 checksummed address.
func GenerateKey() (privateKeyHex, address string, err error) {
	key, err := crypto.GenerateKey()
	if err != nil {
		return "", "", fmt.Errorf("generating key: %w", err)
	}
	return hex.EncodeToString(crypto.FromECDSA(key)), addressOf(key), nil
}

// AddressFromPrivateKeyHex derives the checksummed address from a hex private key (with or
// without 0x). Errors never include the key material.
func AddressFromPrivateKeyHex(privateKeyHex string) (string, error) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(privateKeyHex), "0x"))
	if err != nil {
		return "", ErrInvalidKey
	}
	return addressOf(key), nil
}

func addressOf(key *ecdsa.PrivateKey) string {
	return crypto.PubkeyToAddress(key.PublicKey).Hex()
}
