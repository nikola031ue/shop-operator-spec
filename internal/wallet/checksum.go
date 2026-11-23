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
	"regexp"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

var (
	// ErrInvalidAddress is returned for anything that is not 0x followed by 40 hex digits.
	ErrInvalidAddress = errors.New("invalid address: want 0x followed by 40 hex digits")
	// ErrBadChecksum is returned for a mixed-case address whose EIP-55 checksum is wrong.
	ErrBadChecksum = errors.New("address checksum (EIP-55) does not match")

	addressRE = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
)

// NormalizeAddress validates a user-supplied address (data-model.md Wallet.spec.address) and
// returns its EIP-55 checksummed form. All-lowercase and all-uppercase hex carry no checksum
// and are accepted; mixed case must match EIP-55 exactly.
func NormalizeAddress(addr string) (string, error) {
	if !addressRE.MatchString(addr) {
		return "", ErrInvalidAddress
	}
	checksummed := common.HexToAddress(addr).Hex()
	digits := addr[2:]
	if digits == strings.ToLower(digits) || digits == strings.ToUpper(digits) {
		return checksummed, nil
	}
	if addr != checksummed {
		return "", ErrBadChecksum
	}
	return checksummed, nil
}
