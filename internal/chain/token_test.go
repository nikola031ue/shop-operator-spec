package chain

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
)

// The contracts/musdt.md interface, checked on the committed bytecode deployed to Anvil.
func TestMockUSDTOnAnvil(t *testing.T) {
	a := startAnvil(t)
	tok := loadToken(t)
	addr := a.deployToken(t, tok)
	ctx := context.Background()

	call := func(t *testing.T, from common.Address, data []byte) ([]byte, error) {
		t.Helper()
		return a.client.CallContract(ctx, ethereum.CallMsg{From: from, To: &addr, Data: data}, nil)
	}
	unpack := func(t *testing.T, method string, out []byte) any {
		t.Helper()
		values, err := tok.abi.Unpack(method, out)
		if err != nil || len(values) != 1 {
			t.Fatalf("unpack %s: %v (%d values)", method, err, len(values))
		}
		return values[0]
	}
	balance := func(t *testing.T, who common.Address) *big.Int {
		t.Helper()
		out, err := call(t, who, tok.pack(t, "balanceOf", who))
		if err != nil {
			t.Fatalf("balanceOf: %v", err)
		}
		return unpack(t, "balanceOf", out).(*big.Int)
	}

	t.Run("name, symbol and 6 decimals", func(t *testing.T) {
		for method, want := range map[string]any{"name": "Mock USDT", "symbol": "mUSDT", "decimals": uint8(TokenDecimals)} {
			out, err := call(t, a.addrs[0], tok.pack(t, method))
			if err != nil {
				t.Fatalf("%s: %v", method, err)
			}
			if got := unpack(t, method, out); got != want {
				t.Errorf("%s = %v, want %v", method, got, want)
			}
		}
	})

	t.Run("anyone mints up to 1,000 mUSDT to themselves", func(t *testing.T) {
		a.send(t, 1, &addr, tok.pack(t, "mint", maxMint))
		if got := balance(t, a.addrs[1]); got.Cmp(maxMint) != 0 {
			t.Fatalf("balance after mint = %s, want %s", got, maxMint)
		}
		if got := balance(t, a.addrs[0]); got.Sign() != 0 {
			t.Fatalf("minting credited someone else: account 0 has %s", got)
		}
	})

	t.Run("more than 1,000 mUSDT in one call reverts with MintTooLarge", func(t *testing.T) {
		tooMuch := new(big.Int).Add(maxMint, big.NewInt(1))
		_, err := call(t, a.addrs[1], tok.pack(t, "mint", tooMuch))
		if err == nil {
			t.Fatal("mint above the cap succeeded")
		}
		selector := "0x" + common.Bytes2Hex(tok.abi.Errors["MintTooLarge"].ID.Bytes()[:4])
		var dataErr rpc.DataError
		if !errors.As(err, &dataErr) {
			t.Fatalf("revert without data: %v", err)
		}
		if data, _ := dataErr.ErrorData().(string); !strings.HasPrefix(data, selector) {
			t.Fatalf("revert data %v, want MintTooLarge %s", dataErr.ErrorData(), selector)
		}
	})

	t.Run("transfer emits the Transfer event the watcher reads", func(t *testing.T) {
		value := big.NewInt(11_000_000) // 11.00 mUSDT, an order total of 1100 cents
		receipt := a.send(t, 1, &addr, tok.pack(t, "transfer", a.addrs[0], value))
		if len(receipt.Logs) != 1 {
			t.Fatalf("logs = %d, want 1", len(receipt.Logs))
		}
		log := receipt.Logs[0]
		if log.Address != addr {
			t.Errorf("log address %s, want the token %s", log.Address, addr)
		}
		if len(log.Topics) != 3 || log.Topics[0] != TransferEventTopic {
			t.Fatalf("topics %v, want Transfer + from + to", log.Topics)
		}
		if common.BytesToAddress(log.Topics[1].Bytes()) != a.addrs[1] || common.BytesToAddress(log.Topics[2].Bytes()) != a.addrs[0] {
			t.Errorf("from/to topics %s/%s, want %s/%s", log.Topics[1], log.Topics[2], a.addrs[1], a.addrs[0])
		}
		if got := new(big.Int).SetBytes(log.Data); got.Cmp(value) != 0 {
			t.Errorf("value %s, want %s", got, value)
		}
		if got := balance(t, a.addrs[0]); got.Cmp(value) != 0 {
			t.Errorf("receiver balance %s, want %s", got, value)
		}
	})
}

// The topic and selectors the shop page and the watcher hard-code (contracts/musdt.md).
func TestTokenConstants(t *testing.T) {
	if got, want := TransferEventTopic.Hex(), "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"; got != want {
		t.Errorf("Transfer topic %s, want %s", got, want)
	}
	tok := loadToken(t)
	for method, want := range map[string]string{
		"transfer": "a9059cbb", "balanceOf": "70a08231", "mint": "a0712d68",
	} {
		if got := common.Bytes2Hex(tok.abi.Methods[method].ID); got != want {
			t.Errorf("%s selector %s, want %s", method, got, want)
		}
		if got := common.Bytes2Hex(crypto.Keccak256([]byte(tok.abi.Methods[method].Sig))[:4]); got != want {
			t.Errorf("%s signature hash %s, want %s", method, got, want)
		}
	}
}
