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

### What the shop app receives

Every shop pod gets these environment variables. Secrets are passed by reference only; their
values never appear in the Shop, its status or the operator log.

| Variable | `standard` | `light` | Source |
|----------|-----------|---------|--------|
| `SHOP_NAME` | ✓ | ✓ | `spec.shopName` |
| `WALLET_ADDRESS` | ✓ | ✓ | the referenced Wallet's address |
| `DATABASE_URL` | ✓ | – | CNPG Secret `<shop>-db-app`, key `uri` |
| `REDIS_URL` | – | ✓ | `redis://<shop>-redis.<namespace>.svc:6379` (no credentials) |
| `ADMIN_PASSWORD` | ✓ | ✓ | Secret `<shop>-admin`, key `password` (since 0.2.0) |
| `REDIS_PASSWORD` | – | ✓ | Secret `<shop>-redis-auth` (since 0.2.0; `<shop>-redis-auth-<n>` after a recovery, 0.2.1), key `password` |

The app listens on port 80; readiness is a TCP check on that port. Pods get memory request
128Mi / limit 256Mi, and the sysctl `net.ipv4.ip_unprivileged_port_start=0`, so a non-root app
can bind port 80.

**Generated passwords** (since 0.2.0). For every Shop the operator creates `<shop>-admin` (24
random characters), the shop's initial admin password. For a `light` Shop it also creates
`<shop>-redis-auth` (32 characters), which the shop's Redis requires. Both are owned by the
Shop and created once; an existing password is never replaced. Light-shop Redis keeps its data
on a 1Gi volume with append-only persistence; deleting the Shop deletes the volume.

- A deleted `<shop>-admin` is regenerated, and the pods restart with it. The shop app uses it
  only while the admin still has the generated password.
- A deleted Redis password Secret is regenerated under a new name (since 0.2.1). See below.

### A deleted Redis password Secret (since 0.2.1)

If the Redis password Secret of a `light` Shop is deleted while its Redis exists, the operator
recovers on its own, with no data loss:

1. It creates a new Secret with a new password under the next name: `<shop>-redis-auth` →
   `<shop>-redis-auth-2` → `<shop>-redis-auth-3` … The old name is not reused.
2. It points the Redis at the new Secret. The Redis operator then restarts the Redis pod with
   the new password; the volume and its data stay. The Redis and its volume are never deleted
   for this.
3. The shop pods roll, because their `REDIS_PASSWORD` reference changes.

While the Redis restarts, the Shop reports `Ready=False` with reason `DatabaseProvisioning`. The
log line `Redis password Secret is missing; restarting Redis with a new password Secret` names
both Secrets, never a password. Find the current Secret in the Redis resource:

```bash
kubectl get redis <shop>-redis -n <namespace> -o jsonpath='{.spec.kubernetesConfig.redisSecret.name}'
```

0.2.0 reported `RedisCredentialsLost` here and needed the Shop to be recreated; 0.2.1 no longer
uses that reason.

### Upgrading from 0.1.0

0.1.0 shops are upgraded automatically: they get their Secrets and new variables, and their pods
roll. A `light` shop's 0.1.0 Redis had no password and no volume. Storage can't be added to it in
place, so the operator deletes it once and creates a new one (log line `Recreating Redis without
storage (0.1.0 upgrade)`). 0.1.0 light shops ran only the placeholder app, so no real data is
lost. `standard` shops keep their database.

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
2. Create the token Secret once per cluster, in the operator's namespace. This is a manual
   step (plan Complexity Tracking): the token must never be committed.

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
| `--discord-token-secret` | `discord.tokenSecretName` | `shop-operator-discord` | Token Secret (key `token`) in the operator namespace |
| env `POD_NAMESPACE` | set by the chart | `shop-operator-system` | Operator namespace |
| `--watch-namespace` | `shopsNamespace` | `shops` | The only namespace whose Shops and Wallets are reconciled and cached |
| `--shop-domain` | `shopDomain` | `shops.localhost` | A Shop is served at `https://<shop>.<shop-domain>` (`status.url`) |
| `--ingress-class` | `ingressClassName` | `traefik` | `spec.ingressClassName` of every Shop Ingress |
| `--ingress-namespace` | `ingressNamespace` | `traefik` | Entry point namespace, admitted by the Shop NetworkPolicy |
| `--shophub-namespace` | `shophubNamespace` | `shophub` | ShopHub namespace; the Shop NetworkPolicy admits the ShopHub app pods there |
| `--shophub-pod-selector` | `shophubPodLabels` | `app.kubernetes.io/name=shophub,app.kubernetes.io/instance=shophub` | Labels of the ShopHub app pods; only they are admitted from the ShopHub namespace |
| `--trusted-proxy-host` | `trustedProxyHost` | `traefik-proxies.traefik.svc.cluster.local` | Passed to the shop app as `TRUSTED_PROXY_HOST`: forwarded headers count only from the addresses this name resolves to (the Traefik pods). Empty trusts no proxy |

### Namespaces and permissions (0.3.0)

The operator has no cluster-wide permissions:

- **Shops namespace** (`--watch-namespace`): a Role for every kind it reconciles: Shops,
  Wallets, DiscordChannels (with status and finalizers), Deployments, Services, Secrets, CNPG
  Clusters, Redis, StatefulSets (read), Ingresses and NetworkPolicies. Only Secrets labelled
  `app.kubernetes.io/managed-by=shop-operator` are cached.
- **Operator namespace**: a Role for DiscordChannels and Secrets, for the platform alerts
  channel and its webhook Secret, and for reading the Discord token Secret. Shops and Wallets
  there are not reconciled. Plus the leader-election Role.

Shops, Wallets and DiscordChannels in any other namespace are ignored. Each Shop gets two more
owned children: an Ingress for `<shop>.<shop-domain>` (no `tls` block; the entry point's default
certificate covers it) and a NetworkPolicy that admits TCP 80 only from the entry point and
ShopHub namespaces.

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
