# shop-operator

Kubernetes operator for ShopHub (Requirements 3.1, 3.2). It turns three custom resources into
running infrastructure:

| Kind | What the operator does |
|------|------------------------|
| `Shop` | Runs the shop application (2 replicas for `standard`, 3 for `high` availability), a ClusterIP Service, and a PostgreSQL (CloudNativePG, `database: standard`) or Redis (OT redis-operator, `database: light`) database. Passes the shop name, wallet address and database connection to the app. Deleting the Shop deletes all of it, including the data. |
| `Wallet` | Registers an Ethereum Sepolia address you own (`spec.address`), or generates a new account when `spec.address` is omitted. Fully offline: no blockchain node is contacted. |
| `DiscordChannel` | Creates a text channel and webhook on the project Discord server and stores the webhook URL in a Secret. Deleting the resource deletes the channel. |

Specification, plan and contracts: `shophub-specs/specs/002-shop-operator` (API reference:
`contracts/crds.md`). The operator is installed by the `shop-operator` Helm chart in
`helm-charts`, deployed from `kube-state`.

> Until feature 3 ships the real Shop application, Shops run a configurable placeholder image
> (`nginx:1.27.3` by default).

## Resources

API group `shophub.io/v1alpha1`, all namespaced. A Shop references a Wallet and a
DiscordChannel in its namespace; the ShopHub backend creates all three (constitution
Principle V).

```yaml
apiVersion: shophub.io/v1alpha1
kind: Wallet
metadata: { name: acme-wallet }
spec: {}                        # or: address: "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"
---
apiVersion: shophub.io/v1alpha1
kind: DiscordChannel
metadata: { name: acme-alerts }
spec: { channelName: acme-alerts }
---
apiVersion: shophub.io/v1alpha1
kind: Shop
metadata:
  name: acme
  labels: { shophub.io/owner: user-42, shophub.io/shop: acme }
spec:
  shopName: "Acme Clothing"
  availability: standard        # standard (2 replicas) | high (3 replicas)
  database: standard            # standard (PostgreSQL) | light (Redis); immutable
  walletRef: { name: acme-wallet }
  discordChannelRef: { name: acme-alerts }
```

Every resource reports a `Ready` condition with a reason code: `kubectl get shop,wallet,dch`
shows them. Reason codes and invalid inputs: `contracts/crds.md`.

A Shop is Ready only when its Wallet and DiscordChannel are Ready, its database is ready and
all copies are available. Its status shows the in-cluster URL, the wallet address in use and
the webhook Secret name.

## Wallets and private keys

- **Registered** (`spec.address` set): only the address is stored. A mixed-case address must
  carry a valid EIP-55 checksum (reason `InvalidChecksum` otherwise).
- **Generated** (`spec.address` omitted): the operator generates a key **once** into Secret
  `<wallet>-wallet-key` (key `privateKey`), owned by the Wallet. It never regenerates while
  that Secret exists, and the key never appears in status, events or logs.

> ⚠️ **Deleting a generated Wallet deletes its private key.** Funds on that address become
> unreachable. This is acceptable only because ShopHub uses testnet tokens (Sepolia, mUSDT).
> A Wallet that a Shop still references cannot be deleted: deletion waits (reason `InUse`)
> until no Shop references it.

### Recovering from `KeyLost`

If the `<wallet>-wallet-key` Secret of a generated Wallet is deleted by hand, the key is gone.
The Wallet reports `KeyLost` and keeps showing the old address. It does **not** generate a new
key, because that would silently change the payment address. To recover:

1. Create a new Wallet (new key, new address).
2. Point every Shop that used the old Wallet at the new one (`spec.walletRef.name`). The shops
   roll to the new address.
3. Delete the old Wallet. Its in-use protection lets the deletion through once no Shop
   references it.
4. To reuse the old name, create a Wallet under it only after that deletion has finished.

Funds sent to the old address stay unreachable.

## Discord setup

1. Create a Discord application with a bot and invite it to the project server with the
   **Manage Channels** and **Manage Webhooks** permissions.
2. Create the token Secret once per cluster, in the operator's namespace. This is the one
   manual step (plan Complexity Tracking): the token must never be committed.

   ```bash
   kubectl create secret generic shop-operator-discord -n shop-operator-system \
     --from-literal=token="$DISCORD_BOT_TOKEN"
   ```

3. Set the server ID in the chart values (`discord.guildID`, flag `--discord-guild-id`).

Without these, the operator still runs. DiscordChannels report `DiscordCredentialsMissing`,
and the token Secret is re-checked every minute.

## Configuration

| Flag | Chart value | Default | Meaning |
|------|-------------|---------|---------|
| `--shop-image` | `shopImage.repository` | `nginx` | Placeholder Shop app image |
| `--shop-image-tag` | `shopImage.tag` | `1.27.3` | Must be pinned; `latest` is refused at startup |
| `--discord-guild-id` | `discord.guildID` | `""` | Discord server ID |
| `--discord-token-secret` | `discord.tokenSecret` | `shop-operator-discord` | Token Secret (key `token`) in the operator namespace |
| env `POD_NAMESPACE` | set by the chart | `shop-operator-system` | Operator namespace |

Database operators are detected at startup. On a cluster without CloudNativePG or the OT
redis-operator the operator still starts, and Shops that need the missing database report
`DatabaseOperatorMissing`. Install the operator, then restart shop-operator.

## Development

Requirements: Go (version in `go.mod`) and Docker. kubebuilder has no Windows build, so on
Windows run the targets in a Linux container or WSL. CI runs them on Ubuntu.

```bash
make manifests generate   # CRDs, RBAC and deepcopy from the markers in api/ and internal/
make test                 # unit tests + envtest integration tests
make lint                 # golangci-lint
hack/fetch-crds.sh        # refresh upstream CNPG/Redis CRDs in test/crds (pinned versions)
```

- `internal/wallet` — offline key generation and EIP-55 checks
- `internal/resources` — pure builders for the shop's child objects
- `internal/discord` — minimal Discord REST client
- `internal/controller` — the three reconcilers (envtest specs alongside)

## Releases

Images are published to `ghcr.io/nikola031ue/shop-operator:X.Y.Z` by the `release` CI job when
a `vX.Y.Z` tag is pushed on `main`. The job requires `lint` and `test` to pass, refuses
non-SemVer tags and never overwrites an existing version. `main` is protected: PRs only, CI
required, linear history (constitution Principle II).

## License

Apache License 2.0.
