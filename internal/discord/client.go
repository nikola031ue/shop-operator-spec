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

// Package discord is a minimal client for the four Discord REST v10 calls the DiscordChannel
// reconciler needs (research R9). Errors never contain the bot token or a webhook URL.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DefaultBaseURL is the Discord REST API v10 endpoint.
const DefaultBaseURL = "https://discord.com/api/v10"

// Channel is the subset of a Discord channel the operator uses.
type Channel struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Topic string `json:"topic"`
	Type  int    `json:"type"`
}

// Client is the Discord API surface used by the reconciler; tests substitute a fake.
type Client interface {
	ListChannels(ctx context.Context, guildID string) ([]Channel, error)
	CreateChannel(ctx context.Context, guildID, name, topic string) (Channel, error)
	// CreateWebhook returns the webhook URL. Treat it as a secret.
	CreateWebhook(ctx context.Context, channelID, name string) (string, error)
	// DeleteChannel succeeds if the channel is already gone (404).
	DeleteChannel(ctx context.Context, channelID string) error
}

// RetryableError is a rate limit (429), a server error (5xx) or a network failure.
type RetryableError struct {
	Op         string
	Status     int // 0 for network errors
	RetryAfter time.Duration
}

func (e *RetryableError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("discord %s: network error", e.Op)
	}
	return fmt.Sprintf("discord %s: HTTP %d (retry after %s)", e.Op, e.Status, e.RetryAfter)
}

// ForbiddenError is a 401/403: wrong token or missing bot permissions. Not retried quickly.
type ForbiddenError struct {
	Op     string
	Status int
}

func (e *ForbiddenError) Error() string {
	return fmt.Sprintf("discord %s: HTTP %d (check the bot token and its Manage Channels / Manage Webhooks permissions)", e.Op, e.Status)
}

// HTTPClient implements Client over net/http.
type HTTPClient struct {
	BaseURL   string
	Token     string
	HTTP      *http.Client
	UserAgent string
}

// NewHTTPClient returns a client for the public Discord API.
func NewHTTPClient(token string) *HTTPClient {
	return &HTTPClient{
		BaseURL:   DefaultBaseURL,
		Token:     token,
		HTTP:      &http.Client{Timeout: 15 * time.Second},
		UserAgent: "DiscordBot (https://github.com/nikola031ue/shop-operator-spec, 0.1.0)",
	}
}

// ListChannels returns all channels of a guild.
func (c *HTTPClient) ListChannels(ctx context.Context, guildID string) ([]Channel, error) {
	var out []Channel
	err := c.do(ctx, "list channels", http.MethodGet, "/guilds/"+guildID+"/channels", nil, &out)
	return out, err
}

// CreateChannel creates a text channel (type 0).
func (c *HTTPClient) CreateChannel(ctx context.Context, guildID, name, topic string) (Channel, error) {
	var out Channel
	body := map[string]any{"name": name, "type": 0, "topic": topic}
	err := c.do(ctx, "create channel", http.MethodPost, "/guilds/"+guildID+"/channels", body, &out)
	return out, err
}

// CreateWebhook creates a webhook on a channel and returns its URL.
func (c *HTTPClient) CreateWebhook(ctx context.Context, channelID, name string) (string, error) {
	var out struct {
		ID    string `json:"id"`
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	if err := c.do(ctx, "create webhook", http.MethodPost, "/channels/"+channelID+"/webhooks",
		map[string]any{"name": name}, &out); err != nil {
		return "", err
	}
	if out.URL != "" {
		return out.URL, nil
	}
	if out.ID == "" || out.Token == "" {
		return "", errors.New("discord create webhook: response has no webhook URL")
	}
	return "https://discord.com/api/webhooks/" + out.ID + "/" + out.Token, nil
}

// DeleteChannel deletes a channel; 404 counts as success.
func (c *HTTPClient) DeleteChannel(ctx context.Context, channelID string) error {
	err := c.do(ctx, "delete channel", http.MethodDelete, "/channels/"+channelID, nil, nil)
	var se *statusError
	if errors.As(err, &se) && se.status == http.StatusNotFound {
		return nil
	}
	return err
}

// statusError is any other non-2xx response. It carries Discord's error code/message only.
type statusError struct {
	op      string
	status  int
	code    int
	message string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("discord %s: HTTP %d (code %d: %s)", e.op, e.status, e.code, e.message)
}

func (c *HTTPClient) do(ctx context.Context, op, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return fmt.Errorf("discord %s: building request", op)
	}
	req.Header.Set("Authorization", "Bot "+c.Token)
	req.Header.Set("User-Agent", c.UserAgent)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// The underlying error may echo the URL but never headers; still report only the op.
		return &RetryableError{Op: op}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if out == nil || len(raw) == 0 {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("discord %s: decoding response", op)
		}
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return &RetryableError{Op: op, Status: resp.StatusCode, RetryAfter: retryAfter(resp, raw)}
	case resp.StatusCode >= 500:
		return &RetryableError{Op: op, Status: resp.StatusCode}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &ForbiddenError{Op: op, Status: resp.StatusCode}
	}
	var de struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &de)
	return &statusError{op: op, status: resp.StatusCode, code: de.Code, message: de.Message}
}

// retryAfter reads Discord's retry_after (seconds, float) from the body, else the header.
func retryAfter(resp *http.Response, raw []byte) time.Duration {
	var b struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(raw, &b) == nil && b.RetryAfter > 0 {
		return time.Duration(b.RetryAfter * float64(time.Second))
	}
	if s, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil && s > 0 {
		return time.Duration(s * float64(time.Second))
	}
	return time.Second
}
