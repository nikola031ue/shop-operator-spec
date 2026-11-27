package chain

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
	"github.com/nikola031ue/shop-operator/internal/controller"
	"github.com/nikola031ue/shop-operator/internal/shopapi"
)

const (
	testNS       = "shops"
	operatorNS   = "shop-operator-system"
	shopToken    = "internal-token-0123456789abcdefghijklmnopqrstu"
	shopAddrText = "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"
)

// fakeShop is a shop's internal API: it records deliveries and answers 204 unless told to fail.
type fakeShop struct {
	mu         sync.Mutex
	fail       bool
	deliveries []shopapi.Delivery
	server     *httptest.Server
}

func newFakeShop(t *testing.T) *fakeShop {
	s := &fakeShop{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+shopToken || r.URL.Path != "/internal/chain" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if s.fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var d shopapi.Delivery
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.deliveries = append(s.deliveries, d)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *fakeShop) transfers() []shopapi.Transfer {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []shopapi.Transfer
	for _, d := range s.deliveries {
		all = append(all, d.Transfers...)
	}
	return all
}

func (s *fakeShop) last() shopapi.Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deliveries[len(s.deliveries)-1]
}

// recordingReader remembers every log query's block range.
type recordingReader struct {
	Reader
	mu     sync.Mutex
	ranges [][2]uint64
}

func (r *recordingReader) FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	r.mu.Lock()
	r.ranges = append(r.ranges, [2]uint64{q.FromBlock.Uint64(), q.ToBlock.Uint64()})
	r.mu.Unlock()
	return r.Reader.FilterLogs(ctx, q)
}

type harness struct {
	a       *anvil
	tok     token
	token   common.Address
	shop    *fakeShop
	k8s     client.Client
	watcher *Watcher
	reader  *recordingReader
	wallet  ktypes.NamespacedName
}

func newHarness(t *testing.T, rangeSize uint64, withRPCSecret bool) *harness {
	t.Helper()
	a := startAnvil(t)
	tok := loadToken(t)
	tokenAddr := a.deployToken(t, tok)
	a.send(t, 1, &tokenAddr, tok.pack(t, "mint", maxMint))

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := shophubv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	wl := &shophubv1alpha1.Wallet{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-w1", Namespace: testNS, Labels: map[string]string{"shophub.io/shop": "acme"}},
		Spec:       shophubv1alpha1.WalletSpec{Address: shopAddrText},
		Status:     shophubv1alpha1.WalletStatus{Address: shopAddrText},
	}
	objects := []client.Object{wl, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-internal", Namespace: testNS},
		Data:       map[string][]byte{"token": []byte(shopToken)},
	}}
	endpoint := a.endpoint
	if withRPCSecret {
		objects = append(objects, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "shop-operator-rpc", Namespace: operatorNS},
			Data:       map[string][]byte{"url": []byte(endpoint)},
		})
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&shophubv1alpha1.Wallet{}).Build()

	shop := newFakeShop(t)
	shops := shopapi.NewHTTPClient()
	shops.BaseURL = func(shopapi.Shop) string { return shop.server.URL }

	payments := controller.DefaultPayments(operatorNS)
	payments.TokenAddress = tokenAddr.Hex()
	payments.ChainID = 31337 // Anvil's chain id (startAnvil)
	payments.Confirmations = 3
	payments.LogsBlockRange = rangeSize
	h := &harness{a: a, tok: tok, token: tokenAddr, shop: shop, k8s: k8s,
		wallet: ktypes.NamespacedName{Namespace: testNS, Name: "acme-w1"}}
	h.watcher = &Watcher{
		Client: k8s, APIReader: k8s, Shops: shops, Log: logr.Discard(),
		Config: controller.Config{WatchNamespace: testNS, Payments: payments},
		Dial: func(ctx context.Context, endpoint string) (Reader, error) {
			r, err := DialEthclient(ctx, endpoint)
			h.reader = &recordingReader{Reader: r}
			return h.reader, err
		},
	}
	return h
}

func (h *harness) pay(t *testing.T, value int64) common.Hash {
	t.Helper()
	r := h.a.send(t, 1, &h.token, h.tok.pack(t, "transfer", common.HexToAddress(shopAddrText), big.NewInt(value)))
	return r.TxHash
}

func (h *harness) mine(t *testing.T, n int) {
	t.Helper()
	if err := h.a.client.Client().Call(nil, "anvil_mine", n); err != nil {
		t.Fatalf("anvil_mine: %v", err)
	}
}

func (h *harness) head(t *testing.T) uint64 {
	t.Helper()
	n, err := h.a.client.BlockNumber(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *harness) payments(t *testing.T) shophubv1alpha1.WalletPayments {
	t.Helper()
	var wl shophubv1alpha1.Wallet
	if err := h.k8s.Get(context.Background(), h.wallet, &wl); err != nil {
		t.Fatal(err)
	}
	if wl.Status.Payments == nil {
		return shophubv1alpha1.WalletPayments{}
	}
	return *wl.Status.Payments
}

// A transfer is delivered once, only after the confirmations, with its block time and value;
// progress is delivered on later passes without repeating it (research R2, R3).
func TestWatcherDeliversConfirmedTransfersOnce(t *testing.T) {
	h := newHarness(t, 10, true)
	ctx := context.Background()
	h.mine(t, 5)

	h.watcher.Pass(ctx) // starts the Wallet at head − 3
	start := h.payments(t).ScannedThroughBlock
	if want := int64(h.head(t) - 3); start != want {
		t.Fatalf("cursor starts at %d, want %d", start, want)
	}

	tx := h.pay(t, 11_000_000)
	paidAt := h.head(t)
	h.mine(t, 2) // only 2 blocks after: not confirmed
	h.watcher.Pass(ctx)
	if got := h.shop.transfers(); len(got) != 0 {
		t.Fatalf("delivered before 3 confirmations: %+v", got)
	}

	h.mine(t, 1)
	h.watcher.Pass(ctx)
	got := h.shop.transfers()
	if len(got) != 1 {
		t.Fatalf("transfers = %+v", got)
	}
	tr := got[0]
	if tr.Tx != tx.Hex() || tr.Block != paidAt || tr.Value != "11000000" || tr.To != shopAddrText || tr.From != h.a.addrs[1].Hex() {
		t.Fatalf("transfer = %+v", tr)
	}
	if tr.BlockTime.IsZero() || h.shop.last().SeenThrough.Block != paidAt || h.shop.last().Address != shopAddrText {
		t.Fatalf("delivery = %+v", h.shop.last())
	}
	if p := h.payments(t); p.ScannedThroughBlock != int64(paidAt) || p.ScannedThroughTime == nil || p.LastError != "" {
		t.Fatalf("status = %+v", p)
	}

	h.mine(t, 2)
	h.watcher.Pass(ctx)
	if len(h.shop.transfers()) != 1 || h.shop.last().SeenThrough.Block != paidAt+2 {
		t.Fatalf("heartbeat must move progress without repeating the transfer: %+v", h.shop.last())
	}
}

// Catch-up after a gap pages through ranges of at most LogsBlockRange blocks (free plan limits)
// and loses nothing (FR-008, FR-020).
func TestWatcherCatchesUpInSmallRanges(t *testing.T) {
	h := newHarness(t, 2, true)
	ctx := context.Background()
	h.mine(t, 5)
	h.watcher.Pass(ctx)

	sent := make([]common.Hash, 0, 4)
	for i := range 4 {
		sent = append(sent, h.pay(t, int64(1_000_000*(i+1))))
		h.mine(t, 2)
	}
	h.mine(t, 3)
	for range 10 {
		if h.watcher.Pass(ctx) != time.Second {
			break
		}
	}

	got := h.shop.transfers()
	if len(got) != len(sent) {
		t.Fatalf("delivered %d transfers, want %d: %+v", len(got), len(sent), got)
	}
	for i, tr := range got {
		if tr.Tx != sent[i].Hex() {
			t.Fatalf("transfer %d = %s, want %s", i, tr.Tx, sent[i].Hex())
		}
	}
	for _, r := range h.reader.ranges {
		if r[1]-r[0]+1 > 2 {
			t.Fatalf("a log query covered %d blocks: %v", r[1]-r[0]+1, r)
		}
	}
	if p := h.payments(t); p.ScannedThroughBlock != int64(h.head(t)-3) {
		t.Fatalf("cursor %d, want %d", p.ScannedThroughBlock, h.head(t)-3)
	}
}

// A shop that fails keeps its Wallet's cursor and records ShopUnreachable; once it answers, the
// same range is delivered again and the error is cleared (at-least-once; the shop is idempotent).
func TestWatcherHoldsTheCursorWhileTheShopFails(t *testing.T) {
	h := newHarness(t, 10, true)
	ctx := context.Background()
	h.mine(t, 5)
	h.watcher.Pass(ctx)
	before := h.payments(t).ScannedThroughBlock

	h.shop.fail = true
	tx := h.pay(t, 5_000_000)
	h.mine(t, 3)
	h.watcher.Pass(ctx)
	p := h.payments(t)
	if p.ScannedThroughBlock != before || p.LastError != shophubv1alpha1.PaymentsReasonShopUnreachable {
		t.Fatalf("status while the shop fails = %+v (cursor was %d)", p, before)
	}

	h.shop.fail = false
	h.watcher.Pass(ctx)
	if got := h.shop.transfers(); len(got) != 1 || got[0].Tx != tx.Hex() {
		t.Fatalf("transfers after recovery = %+v", got)
	}
	if p := h.payments(t); p.LastError != "" || p.ScannedThroughBlock <= before {
		t.Fatalf("status after recovery = %+v", p)
	}
}

// Without the hand-made RPC Secret the watcher records RPCCredentialsMissing; a provider URL never
// reaches status (it carries an API key).
func TestWatcherReportsMissingCredentialsAndHidesTheURL(t *testing.T) {
	h := newHarness(t, 10, false)
	ctx := context.Background()

	h.watcher.Pass(ctx)
	if p := h.payments(t); p.LastError != shophubv1alpha1.PaymentsReasonRPCCredentialsMissing || p.ScannedThroughBlock != 0 {
		t.Fatalf("status = %+v", p)
	}

	if err := h.k8s.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-operator-rpc", Namespace: operatorNS},
		Data:       map[string][]byte{"url": []byte("http://127.0.0.1:1/v2/key-SECRET-123")},
	}); err != nil {
		t.Fatal(err)
	}
	h.watcher.Dial = DialEthclient // unreachable endpoint: dials lazily, fails on the first call
	h.watcher.Pass(ctx)
	p := h.payments(t)
	if p.LastError != shophubv1alpha1.PaymentsReasonProviderError {
		t.Fatalf("status = %+v", p)
	}
	if strings.Contains(p.LastErrorMessage, "key-SECRET") {
		t.Fatalf("the provider URL leaked into status: %q", p.LastErrorMessage)
	}
}

// An endpoint on another chain (e.g. a mainnet URL in the RPC Secret) is reported as WrongChain
// and nothing is read; the reader isn't kept, so once the endpoint is right the next pass works.
func TestWatcherRefusesAProviderOnTheWrongChain(t *testing.T) {
	h := newHarness(t, 10, true)
	ctx := context.Background()
	h.mine(t, 5)
	h.watcher.Config.Payments.ChainID = 424242 // any id but Anvil's 31337

	h.watcher.Pass(ctx)
	p := h.payments(t)
	if p.LastError != shophubv1alpha1.PaymentsReasonWrongChain || p.ScannedThroughBlock != 0 {
		t.Fatalf("status on the wrong chain = %+v", p)
	}
	if !strings.Contains(p.LastErrorMessage, "31337") || !strings.Contains(p.LastErrorMessage, "424242") {
		t.Fatalf("the message should name both chain ids: %q", p.LastErrorMessage)
	}
	if strings.Contains(p.LastErrorMessage, h.a.endpoint) {
		t.Fatalf("the provider URL leaked into status: %q", p.LastErrorMessage)
	}
	if len(h.reader.ranges) != 0 {
		t.Fatalf("logs were read from the wrong chain: %v", h.reader.ranges)
	}

	h.watcher.Config.Payments.ChainID = 31337
	h.watcher.Pass(ctx)
	if p := h.payments(t); p.LastError != "" || p.ScannedThroughBlock != int64(h.head(t)-3) {
		t.Fatalf("status once the chain is right = %+v", p)
	}
}

// A read position above the head (written against another chain) is an error, not a wait: the
// Wallet reports CursorAheadOfChain, keeps its value, gets no deliveries, and restarts at the
// confirmed head once an admin clears status.payments.
func TestWatcherReportsAReadPositionAboveTheHead(t *testing.T) {
	h := newHarness(t, 10, true)
	ctx := context.Background()
	h.mine(t, 5)
	h.watcher.Pass(ctx)

	ahead := int64(h.head(t) + 1000)
	var wl shophubv1alpha1.Wallet
	if err := h.k8s.Get(ctx, h.wallet, &wl); err != nil {
		t.Fatal(err)
	}
	wl.Status.Payments.ScannedThroughBlock = ahead
	if err := h.k8s.Status().Update(ctx, &wl); err != nil {
		t.Fatal(err)
	}
	h.pay(t, 7_000_000)
	h.mine(t, 3)

	h.watcher.Pass(ctx)
	p := h.payments(t)
	if p.LastError != shophubv1alpha1.PaymentsReasonCursorAheadOfChain || p.ScannedThroughBlock != ahead {
		t.Fatalf("status with the read position above the head = %+v", p)
	}
	if !strings.Contains(p.LastErrorMessage, "above the chain head") {
		t.Fatalf("message = %q", p.LastErrorMessage)
	}
	if got := h.shop.transfers(); len(got) != 0 {
		t.Fatalf("delivered while the read position is wrong: %+v", got)
	}

	if err := h.k8s.Get(ctx, h.wallet, &wl); err != nil {
		t.Fatal(err)
	}
	wl.Status.Payments = nil
	if err := h.k8s.Status().Update(ctx, &wl); err != nil {
		t.Fatal(err)
	}
	h.watcher.Pass(ctx)
	if p := h.payments(t); p.LastError != "" || p.ScannedThroughBlock != int64(h.head(t)-3) {
		t.Fatalf("status after clearing = %+v", p)
	}
}
