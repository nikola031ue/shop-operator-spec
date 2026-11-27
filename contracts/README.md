# mUSDT — the project's test token

`MockUSDT.sol` is the ShopHub test token on Ethereum Sepolia (spec 005, constitution Principle IV):
an OpenZeppelin ERC20 with **6 decimals** and an **open mint** of at most **1,000 mUSDT per call**
to the caller. It has no value, no owner, no pause and no blacklist. Shops are paid in it; the
shop-operator's payment watcher reads its `Transfer` events.

| File | What |
|------|------|
| `MockUSDT.sol` | the source |
| `package.json`, `package-lock.json` | pin `@openzeppelin/contracts` **5.1.0** |
| `build.sh` | compiles with the pinned compiler into `build/` (Docker) |
| `build/MockUSDT.abi`, `build/MockUSDT.bin` | committed output; CI rebuilds and fails on any difference; tests deploy this bytecode on Anvil |

## Compiler settings

| Setting | Value |
|---------|-------|
| Compiler | `solc` **0.8.28** (image `ethereum/solc:0.8.28`) |
| Optimizer | enabled, **200** runs |
| EVM version | **cancun** |
| OpenZeppelin Contracts | **5.1.0** |

```bash
cd contracts
npm ci
./build.sh            # writes build/MockUSDT.abi and build/MockUSDT.bin
```

## Tests

`internal/chain/token_test.go` starts Anvil (Docker, `ghcr.io/foundry-rs/foundry:v1.0.0`),
deploys `build/MockUSDT.bin` and checks name, symbol, decimals, the mint cap (`MintTooLarge`),
`transfer` and the `Transfer` event. No test ever uses Sepolia (`hack/check-no-chain-hosts.sh`).

## Deploying on Sepolia (manual, maintainer, once)

Deployed by hand from the maintainer's own MetaMask with [Remix](https://remix.ethereum.org); no
deploy key is stored anywhere (spec 005 clarification). Steps: spec 005 tasks.md T004.

**Deployed address (Sepolia, chain 11155111)**: `0xdF8d5dEb736EBC79f7e99C4c90975d4b8f8d80E2`
([explorer](https://sepolia.etherscan.io/address/0xdF8d5dEb736EBC79f7e99C4c90975d4b8f8d80E2),
[Sourcify](https://repo.sourcify.dev/11155111/0xdF8d5dEb736EBC79f7e99C4c90975d4b8f8d80E2)).
Deployed 2026-11-27 by the maintainer with Remix from `main` at `f54cecf`. It goes into
`kube-state/clusters/local/shop-operator/values.yaml` as `payments.tokenAddress`.

Checked after the deploy:
- the deployed runtime code, with the CBOR metadata stripped, is the runtime part of
  `build/MockUSDT.bin`;
- `name()` "Mock USDT", `symbol()` "mUSDT", `decimals()` 6, `MAX_MINT()` 1,000,000,000,
  `totalSupply()` 0;
- Sourcify: exact match (creation and runtime).
