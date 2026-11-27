// Package chain reads the mUSDT token on Ethereum (spec 005): the payment watcher and its
// helpers. It never holds a private key; reading the chain needs none (FR-018).
package chain

import "github.com/ethereum/go-ethereum/crypto"

// TransferEventTopic is topics[0] of an ERC20 Transfer(address indexed from, address indexed to,
// uint256 value) log (contracts/musdt.md).
var TransferEventTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// TokenDecimals is mUSDT's decimals: an amount of x.yz mUSDT is x.yz × 10^6 base units.
const TokenDecimals = 6
