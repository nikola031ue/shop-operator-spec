package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
	"github.com/nikola031ue/shop-operator/internal/controller"
	"github.com/nikola031ue/shop-operator/internal/shopapi"
)

// Reader is the part of an Ethereum JSON-RPC client the watcher uses (ethclient implements it).
type Reader interface {
	BlockNumber(ctx context.Context) (uint64, error)
	FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
}

// DialFunc connects to the provider endpoint.
type DialFunc func(ctx context.Context, endpoint string) (Reader, error)

// DialEthclient is the production DialFunc.
func DialEthclient(ctx context.Context, endpoint string) (Reader, error) {
	return ethclient.DialContext(ctx, endpoint)
}

const (
	// maxRangesPerPass bounds one pass (300 blocks with 10-block ranges, about an hour of
	// Sepolia); a longer catch-up continues on the next pass right away.
	maxRangesPerPass = 30
	// maxBackoff caps the wait after provider errors.
	maxBackoff = 2 * time.Minute
	// shopLabel names a Wallet's owning shop (set by ShopHub, spec 004).
	shopLabel = "shophub.io/shop"
)

// Watcher is the single chain watcher (spec 005 research R1–R3): one leader-elected loop that
// reads mUSDT Transfer logs to every Wallet address in the shops namespace, only up to
// head − confirmations, in pages of at most LogsBlockRange blocks, and delivers each shop its
// transfers plus how far the chain was read. It writes only Wallet status.payments; the Wallet
// controller derives the PaymentsWatching condition from it.
type Watcher struct {
	Client    client.Client
	APIReader client.Reader
	Config    controller.Config
	Shops     shopapi.Client
	Dial      DialFunc
	Log       logr.Logger
	Now       func() time.Time

	reader    Reader
	readerURL string
	backoff   time.Duration
}

// NeedLeaderElection keeps the watcher to one active operator replica.
func (w *Watcher) NeedLeaderElection() bool { return true }

// Start runs passes until ctx ends (manager.Runnable).
func (w *Watcher) Start(ctx context.Context) error {
	if !w.Config.Payments.Enabled() {
		w.Log.Info("Payment watcher idle: no token address configured")
		<-ctx.Done()
		return nil
	}
	w.Log.Info("Payment watcher started", "token", w.Config.Payments.TokenAddress,
		"confirmations", w.Config.Payments.Confirmations, "logsBlockRange", w.Config.Payments.LogsBlockRange)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		timer.Reset(w.Pass(ctx))
	}
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Pass runs one watcher pass and returns how long to wait before the next.
func (w *Watcher) Pass(ctx context.Context) time.Duration {
	poll := w.Config.Payments.PollInterval
	wallets, err := w.wallets(ctx)
	if err != nil {
		w.Log.Error(err, "Listing Wallets failed")
		return poll
	}
	if len(wallets) == 0 {
		return poll
	}

	reader, err := w.connect(ctx)
	if err != nil {
		reason := shophubv1alpha1.PaymentsReasonRPCCredentialsMissing
		if !errors.Is(err, errNoRPCSecret) {
			reason = shophubv1alpha1.PaymentsReasonProviderError
		}
		w.markAll(ctx, wallets, reason, err.Error())
		return w.fail(poll)
	}
	head, err := reader.BlockNumber(ctx)
	if err != nil {
		w.markAll(ctx, wallets, shophubv1alpha1.PaymentsReasonProviderError, "reading the chain head failed")
		w.Log.Info("Reading the chain head failed", "error", err.Error())
		return w.fail(poll)
	}
	conf := w.Config.Payments.Confirmations
	if head < conf {
		return poll
	}
	confirmed := head - conf
	headers := map[uint64]*types.Header{}

	// New Wallets start at the confirmed head: nothing before their creation can pay their orders.
	for i := range wallets {
		if cursor(&wallets[i]) == 0 {
			h, err := w.header(ctx, reader, headers, confirmed)
			if err != nil {
				w.markAll(ctx, wallets, shophubv1alpha1.PaymentsReasonProviderError, "reading a block header failed")
				return w.fail(poll)
			}
			w.advance(ctx, &wallets[i], confirmed, blockTime(h))
		}
	}

	from := confirmed + 1
	for i := range wallets {
		from = min(from, uint64(cursor(&wallets[i]))+1)
	}

	failed := map[string]bool{}
	rangeSize := w.Config.Payments.LogsBlockRange
	for n := 0; from <= confirmed && n < maxRangesPerPass; n++ {
		to := min(from+rangeSize-1, confirmed)
		logs, err := reader.FilterLogs(ctx, w.query(wallets, from, to))
		if err != nil {
			w.markAll(ctx, wallets, shophubv1alpha1.PaymentsReasonProviderError, "reading transfer logs failed")
			w.Log.Info("Reading transfer logs failed", "from", from, "to", to, "error", err.Error())
			return w.fail(poll)
		}
		end, err := w.header(ctx, reader, headers, to)
		if err != nil {
			w.markAll(ctx, wallets, shophubv1alpha1.PaymentsReasonProviderError, "reading a block header failed")
			return w.fail(poll)
		}
		for i := range wallets {
			wl := &wallets[i]
			if failed[wl.Name] || uint64(cursor(wl)) >= to {
				continue
			}
			if err := w.deliver(ctx, reader, headers, wl, logs, to, blockTime(end)); err != nil {
				failed[wl.Name] = true
				w.mark(ctx, wl, shophubv1alpha1.PaymentsReasonShopUnreachable, err.Error())
				continue
			}
			w.advance(ctx, wl, to, blockTime(end))
		}
		from = to + 1
	}
	w.backoff = 0
	if from <= confirmed {
		return time.Second // more to catch up; keep going
	}
	return poll
}

// fail doubles the wait after provider errors, from the poll interval up to maxBackoff.
func (w *Watcher) fail(poll time.Duration) time.Duration {
	w.backoff = min(max(w.backoff*2, poll), maxBackoff)
	return w.backoff
}

var errNoRPCSecret = errors.New("the RPC endpoint Secret is missing or has no url")

// connect (re)dials when the endpoint in the hand-made Secret changes. The URL carries an API
// key: only its host is ever logged, and it never goes into status.
func (w *Watcher) connect(ctx context.Context) (Reader, error) {
	ref := w.Config.Payments.RPCSecret
	var sec corev1.Secret
	if err := w.APIReader.Get(ctx, ref, &sec); err != nil {
		return nil, fmt.Errorf("%w: Secret %s/%s", errNoRPCSecret, ref.Namespace, ref.Name)
	}
	endpoint := string(sec.Data["url"])
	if endpoint == "" {
		return nil, fmt.Errorf("%w: Secret %s/%s", errNoRPCSecret, ref.Namespace, ref.Name)
	}
	if w.reader != nil && endpoint == w.readerURL {
		return w.reader, nil
	}
	host := "?"
	if u, err := url.Parse(endpoint); err == nil {
		host = u.Host
	}
	r, err := w.Dial(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("connecting to the chain provider at %s failed", host)
	}
	w.Log.Info("Connected to the chain provider", "host", host)
	w.reader, w.readerURL = r, endpoint
	return r, nil
}

// wallets lists the Wallets with an address in the shops namespace (including ones being
// deleted: payments to an old address are still delivered until its Wallet is gone, D-1).
func (w *Watcher) wallets(ctx context.Context) ([]shophubv1alpha1.Wallet, error) {
	var list shophubv1alpha1.WalletList
	if err := w.Client.List(ctx, &list, client.InNamespace(w.Config.WatchNamespace)); err != nil {
		return nil, err
	}
	out := list.Items[:0]
	for _, wl := range list.Items {
		if wl.Status.Address != "" && wl.Labels[shopLabel] != "" {
			out = append(out, wl)
		}
	}
	slices.SortFunc(out, func(a, b shophubv1alpha1.Wallet) int { return cmpString(a.Name, b.Name) })
	return out, nil
}

func (w *Watcher) query(wallets []shophubv1alpha1.Wallet, from, to uint64) ethereum.FilterQuery {
	recipients := make([]common.Hash, 0, len(wallets))
	for _, wl := range wallets {
		recipients = append(recipients, common.BytesToHash(common.HexToAddress(wl.Status.Address).Bytes()))
	}
	return ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from),
		ToBlock:   new(big.Int).SetUint64(to),
		Addresses: []common.Address{common.HexToAddress(w.Config.Payments.TokenAddress)},
		Topics:    [][]common.Hash{{TransferEventTopic}, nil, recipients},
	}
}

// deliver posts the Wallet's transfers in (cursor, to] and progress through block `to`.
func (w *Watcher) deliver(ctx context.Context, reader Reader, headers map[uint64]*types.Header,
	wl *shophubv1alpha1.Wallet, logs []types.Log, to uint64, toTime time.Time) error {
	after := uint64(cursor(wl))
	address := common.HexToAddress(wl.Status.Address)
	transfers := []shopapi.Transfer{}
	for _, l := range logs {
		if l.Removed || l.BlockNumber <= after || l.BlockNumber > to || len(l.Topics) != 3 ||
			l.Topics[0] != TransferEventTopic || common.BytesToAddress(l.Topics[2].Bytes()) != address {
			continue
		}
		h, err := w.header(ctx, reader, headers, l.BlockNumber)
		if err != nil {
			return fmt.Errorf("reading block %d failed", l.BlockNumber)
		}
		transfers = append(transfers, shopapi.Transfer{
			Tx: l.TxHash.Hex(), LogIndex: l.Index, Block: l.BlockNumber, BlockTime: blockTime(h),
			From: common.BytesToAddress(l.Topics[1].Bytes()).Hex(), To: address.Hex(),
			Value: new(big.Int).SetBytes(l.Data).String(),
		})
	}
	shop, err := shopapi.ShopFor(ctx, w.secretData, wl.Namespace, wl.Labels[shopLabel])
	if err != nil {
		return fmt.Errorf("the shop's internal token is not readable yet: %w", err)
	}
	return w.Shops.Deliver(ctx, shop, shopapi.Delivery{
		Address: address.Hex(), FromBlock: after + 1,
		SeenThrough: shopapi.SeenThrough{Block: to, Time: toTime}, Transfers: transfers,
	})
}

func (w *Watcher) secretData(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	var sec corev1.Secret
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sec); err != nil {
		return nil, err
	}
	return sec.Data, nil
}

func (w *Watcher) header(ctx context.Context, reader Reader, cache map[uint64]*types.Header, n uint64) (*types.Header, error) {
	if h, ok := cache[n]; ok {
		return h, nil
	}
	h, err := reader.HeaderByNumber(ctx, new(big.Int).SetUint64(n))
	if err != nil {
		return nil, err
	}
	cache[n] = h
	return h, nil
}

// advance records that the shop accepted everything through block n, and clears the error.
func (w *Watcher) advance(ctx context.Context, wl *shophubv1alpha1.Wallet, n uint64, at time.Time) {
	orig := wl.DeepCopy()
	t := metav1.NewTime(at)
	wl.Status.Payments = &shophubv1alpha1.WalletPayments{ScannedThroughBlock: int64(n), ScannedThroughTime: &t}
	w.patch(ctx, wl, orig)
}

// mark records a failure reason for one Wallet; the cursor stays.
func (w *Watcher) mark(ctx context.Context, wl *shophubv1alpha1.Wallet, reason, message string) {
	orig := wl.DeepCopy()
	p := shophubv1alpha1.WalletPayments{}
	if wl.Status.Payments != nil {
		p = *wl.Status.Payments.DeepCopy()
	}
	if p.LastError == reason && p.LastErrorMessage == message {
		return // unchanged: don't rewrite status every pass
	}
	now := metav1.NewTime(w.now())
	p.LastError, p.LastErrorMessage, p.LastErrorTime = reason, message, &now
	wl.Status.Payments = &p
	w.patch(ctx, wl, orig)
}

func (w *Watcher) markAll(ctx context.Context, wallets []shophubv1alpha1.Wallet, reason, message string) {
	for i := range wallets {
		w.mark(ctx, &wallets[i], reason, message)
	}
}

func (w *Watcher) patch(ctx context.Context, wl, orig *shophubv1alpha1.Wallet) {
	if err := w.Client.Status().Patch(ctx, wl, client.MergeFrom(orig)); client.IgnoreNotFound(err) != nil {
		w.Log.Error(err, "Updating Wallet payment status failed", "wallet", wl.Name)
	}
}

func cursor(wl *shophubv1alpha1.Wallet) int64 {
	if wl.Status.Payments == nil {
		return 0
	}
	return wl.Status.Payments.ScannedThroughBlock
}

func blockTime(h *types.Header) time.Time { return time.Unix(int64(h.Time), 0).UTC() }

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
