package chain

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// anvilImage is the local chain for every chain test (constitution Principle IV: never Sepolia).
const anvilImage = "ghcr.io/foundry-rs/foundry:v1.0.0"

// Anvil's well-known default accounts 0 and 1: throwaway keys printed by Anvil itself, holding
// value nowhere (constitution Principle VI allows them for local-only tools).
const (
	anvilKey0 = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	anvilKey1 = "59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
)

// anvil is a running local chain with a client and the two default accounts.
type anvil struct {
	client   *ethclient.Client
	endpoint string
	chainID  *big.Int
	keys     [2]*ecdsa.PrivateKey
	addrs    [2]common.Address
}

// startAnvil runs Anvil in a container for the test (automining: every transaction is mined at
// once). It fails the test rather than skipping when Docker is missing, so CI can't pass silently.
func startAnvil(t *testing.T) *anvil {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        anvilImage,
			Entrypoint:   []string{"anvil"},
			Cmd:          []string{"--host", "0.0.0.0", "--chain-id", "31337"},
			ExposedPorts: []string{"8545/tcp"},
			WaitingFor:   wait.ForLog("Listening on").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start anvil: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })

	endpoint, err := c.PortEndpoint(ctx, "8545/tcp", "http")
	if err != nil {
		t.Fatalf("anvil endpoint: %v", err)
	}
	client, err := ethclient.DialContext(ctx, endpoint)
	if err != nil {
		t.Fatalf("dial anvil: %v", err)
	}
	t.Cleanup(client.Close)

	a := &anvil{client: client, endpoint: endpoint, chainID: big.NewInt(31337)}
	for i, hexKey := range []string{anvilKey0, anvilKey1} {
		key, err := crypto.HexToECDSA(hexKey)
		if err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
		a.keys[i] = key
		a.addrs[i] = crypto.PubkeyToAddress(key.PublicKey)
	}
	return a
}

// send signs and sends a transaction from account `from` and returns its receipt.
func (a *anvil) send(t *testing.T, from int, to *common.Address, data []byte) *types.Receipt {
	t.Helper()
	ctx := context.Background()
	nonce, err := a.client.PendingNonceAt(ctx, a.addrs[from])
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}
	gasPrice, err := a.client.SuggestGasPrice(ctx)
	if err != nil {
		t.Fatalf("gas price: %v", err)
	}
	var tx *types.Transaction
	if to == nil {
		tx = types.NewContractCreation(nonce, big.NewInt(0), 3_000_000, gasPrice, data)
	} else {
		tx = types.NewTransaction(nonce, *to, big.NewInt(0), 300_000, gasPrice, data)
	}
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(a.chainID), a.keys[from])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := a.client.SendTransaction(ctx, signed); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Anvil mines at once, but the receipt can lag the send by a moment on a slow runner.
	var receipt *types.Receipt
	for deadline := time.Now().Add(30 * time.Second); ; {
		receipt, err = a.client.TransactionReceipt(ctx, signed.Hash())
		if err == nil {
			break
		}
		if !errors.Is(err, ethereum.NotFound) || time.Now().After(deadline) {
			t.Fatalf("receipt: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("transaction %s reverted", signed.Hash())
	}
	return receipt
}

// token is the committed MockUSDT ABI and bytecode (contracts/build), as deployed by tests.
type token struct {
	abi      abi.ABI
	bytecode []byte
}

func loadToken(t *testing.T) token {
	t.Helper()
	dir := filepath.Join("..", "..", "contracts", "build")
	abiJSON, err := os.ReadFile(filepath.Join(dir, "MockUSDT.abi"))
	if err != nil {
		t.Fatalf("read ABI: %v", err)
	}
	parsed, err := abi.JSON(strings.NewReader(string(abiJSON)))
	if err != nil {
		t.Fatalf("parse ABI: %v", err)
	}
	binHex, err := os.ReadFile(filepath.Join(dir, "MockUSDT.bin"))
	if err != nil {
		t.Fatalf("read bytecode: %v", err)
	}
	return token{abi: parsed, bytecode: common.FromHex(strings.TrimSpace(string(binHex)))}
}

// deployToken deploys MockUSDT from account 0 and returns its address.
func (a *anvil) deployToken(t *testing.T, tok token) common.Address {
	t.Helper()
	receipt := a.send(t, 0, nil, tok.bytecode)
	return receipt.ContractAddress
}

func (tok token) pack(t *testing.T, method string, args ...any) []byte {
	t.Helper()
	data, err := tok.abi.Pack(method, args...)
	if err != nil {
		t.Fatalf("pack %s: %v", method, err)
	}
	return data
}

// maxMint is the token's per-call cap: 1,000 mUSDT in base units.
var maxMint = new(big.Int).Mul(big.NewInt(1000), big.NewInt(1_000_000))
