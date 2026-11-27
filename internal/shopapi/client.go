// Package shopapi calls a shop app's internal API (spec 005 contracts/shop-internal-api.md):
// delivering confirmed transfers and asking how many orders still wait on an address.
package shopapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// InternalPort is the shop app's internal API port (the Service port named "internal").
const InternalPort = 8081

// Timeout bounds every call; a slow shop never holds up the others (contracts/operator.md).
const Timeout = 5 * time.Second

// Transfer is one confirmed mUSDT transfer to the shop's address.
type Transfer struct {
	Tx        string    `json:"tx"`
	LogIndex  uint      `json:"logIndex"`
	Block     uint64    `json:"block"`
	BlockTime time.Time `json:"blockTime"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Value     string    `json:"value"`
}

// SeenThrough is how far the chain has been read for the address.
type SeenThrough struct {
	Block uint64    `json:"block"`
	Time  time.Time `json:"time"`
}

// Delivery is the body of POST /internal/chain.
type Delivery struct {
	Address     string      `json:"address"`
	FromBlock   uint64      `json:"fromBlock"`
	SeenThrough SeenThrough `json:"seenThrough"`
	Transfers   []Transfer  `json:"transfers"`
}

// Shop addresses one shop's internal API.
type Shop struct {
	Name, Namespace, Token string
}

// Client is what the watcher and the Wallet controller need from shops.
type Client interface {
	// Deliver posts transfers and progress; nil only on 204 (everything stored).
	Deliver(ctx context.Context, shop Shop, d Delivery) error
	// Waiting returns how many orders to address are still waiting for payment.
	Waiting(ctx context.Context, shop Shop, address string) (int64, error)
}

// HTTPClient calls shops over HTTP. BaseURL maps a shop to its internal API root; the default
// is the shop Service's internal port, http://<shop>.<namespace>.svc:8081.
type HTTPClient struct {
	HTTP    *http.Client
	BaseURL func(shop Shop) string
}

// NewHTTPClient returns a client with the default timeout and Service URLs.
func NewHTTPClient() *HTTPClient {
	return &HTTPClient{
		HTTP: &http.Client{Timeout: Timeout},
		BaseURL: func(s Shop) string {
			return fmt.Sprintf("http://%s.%s.svc:%d", s.Name, s.Namespace, InternalPort)
		},
	}
}

func (c *HTTPClient) Deliver(ctx context.Context, shop Shop, d Delivery) error {
	if d.Transfers == nil {
		d.Transfers = []Transfer{} // always an array on the wire
	}
	body, err := json.Marshal(d)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, shop, http.MethodPost, "/internal/chain", body)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("shop %s answered %d to /internal/chain", shop.Name, resp.StatusCode)
	}
	return nil
}

func (c *HTTPClient) Waiting(ctx context.Context, shop Shop, address string) (int64, error) {
	resp, err := c.do(ctx, shop, http.MethodGet, "/internal/wallets/"+address+"/waiting", nil)
	if err != nil {
		return 0, err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("shop %s answered %d to the waiting-order count", shop.Name, resp.StatusCode)
	}
	var out struct {
		Waiting *int64 `json:"waiting"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || out.Waiting == nil {
		return 0, fmt.Errorf("shop %s sent no waiting-order count", shop.Name)
	}
	return *out.Waiting, nil
}

func (c *HTTPClient) do(ctx context.Context, shop Shop, method, path string, body []byte) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL(shop)+path, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+shop.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		cancel()
		// The error names the URL (shop Service), never the token.
		return nil, fmt.Errorf("calling shop %s: %w", shop.Name, err)
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	defer c.cancel()
	return c.ReadCloser.Close()
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

// TokenSecretSuffix and TokenKey locate a shop's internal token: Secret <shop>-internal, key
// "token" (spec 005 data-model "Derived Secret"; kept in step with internal/resources).
const (
	TokenSecretSuffix = "-internal"
	TokenKey          = "token"
)

// SecretGetter reads a Secret's data by name (the operator's cached client: these Secrets carry
// its managed-by label).
type SecretGetter func(ctx context.Context, namespace, name string) (map[string][]byte, error)

// ShopFor returns the shop's address for API calls, with its token read from its Secret.
func ShopFor(ctx context.Context, get SecretGetter, namespace, shop string) (Shop, error) {
	data, err := get(ctx, namespace, shop+TokenSecretSuffix)
	if err != nil {
		return Shop{}, err
	}
	token := string(data[TokenKey])
	if token == "" {
		return Shop{}, fmt.Errorf("secret %s%s has no %q", shop, TokenSecretSuffix, TokenKey)
	}
	return Shop{Name: shop, Namespace: namespace, Token: token}, nil
}
