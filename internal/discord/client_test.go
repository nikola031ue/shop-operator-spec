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

package discord

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testToken = "bot-token-must-never-leak"
	testGuild = "111"
)

// newTestClient starts a server with handler h and returns a client pointing at it.
func newTestClient(t *testing.T, h http.HandlerFunc) (*HTTPClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewHTTPClient(testToken)
	c.BaseURL = srv.URL
	return c, srv
}

func noLeak(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaks the token: %v", err)
	}
}

func TestCreateChannelSendsAuthAndBody(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/guilds/"+testGuild+"/channels" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bot "+testToken {
			t.Errorf("Authorization header %q", got)
		}
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "DiscordBot (") {
			t.Errorf("User-Agent %q must follow Discord's DiscordBot format", r.Header.Get("User-Agent"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "acme-alerts" || body["type"] != float64(0) || body["topic"] != "shophub-uid:u1" {
			t.Errorf("body %v", body)
		}
		_, _ = w.Write([]byte(`{"id":"900","name":"acme-alerts","topic":"shophub-uid:u1","type":0}`))
	})
	ch, err := c.CreateChannel(context.Background(), testGuild, "acme-alerts", "shophub-uid:u1")
	if err != nil || ch.ID != "900" || ch.Topic != "shophub-uid:u1" {
		t.Fatalf("got %+v, %v", ch, err)
	}
}

func TestListChannels(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/guilds/"+testGuild+"/channels" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"id":"1","name":"general","type":0},{"id":"2","name":"x","topic":"shophub-uid:u9","type":0}]`))
	})
	chs, err := c.ListChannels(context.Background(), testGuild)
	if err != nil || len(chs) != 2 || chs[1].Topic != "shophub-uid:u9" {
		t.Fatalf("got %+v, %v", chs, err)
	}
}

func TestCreateWebhookURL(t *testing.T) {
	for name, resp := range map[string]string{
		"url field":    `{"id":"7","token":"tok","url":"https://discord.com/api/webhooks/7/tok"}`,
		"id and token": `{"id":"7","token":"tok"}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/channels/900/webhooks" {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
				_, _ = w.Write([]byte(resp))
			})
			url, err := c.CreateWebhook(context.Background(), "900", "shop-operator")
			if err != nil || url != "https://discord.com/api/webhooks/7/tok" {
				t.Fatalf("got %q, %v", url, err)
			}
		})
	}
}

func TestDeleteChannel404IsSuccess(t *testing.T) {
	for status, wantErr := range map[int]bool{http.StatusOK: false, http.StatusNotFound: false, http.StatusBadRequest: true} {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete || r.URL.Path != "/channels/900" {
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(status)
			if status == http.StatusBadRequest {
				_, _ = w.Write([]byte(`{"code":50035,"message":"Invalid Form Body"}`))
			}
		})
		err := c.DeleteChannel(context.Background(), "900")
		if (err != nil) != wantErr {
			t.Fatalf("status %d: err %v, wantErr %v", status, err, wantErr)
		}
		noLeak(t, err)
	}
}

func TestRateLimitIsRetryable(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"You are being rate limited.","retry_after":1.5,"global":false}`))
	})
	_, err := c.ListChannels(context.Background(), testGuild)
	var re *RetryableError
	if !errors.As(err, &re) || re.Status != 429 || re.RetryAfter != 1500*time.Millisecond {
		t.Fatalf("want RetryableError 429 retry 1.5s, got %#v", err)
	}
	noLeak(t, err)
}

func TestRetryAfterHeaderFallback(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := c.ListChannels(context.Background(), testGuild)
	var re *RetryableError
	if !errors.As(err, &re) || re.RetryAfter != 2*time.Second {
		t.Fatalf("want retry after 2s from header, got %#v", err)
	}
}

func TestServerErrorIsRetryable(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	_, err := c.CreateChannel(context.Background(), testGuild, "a", "b")
	var re *RetryableError
	if !errors.As(err, &re) || re.Status != http.StatusBadGateway {
		t.Fatalf("want RetryableError 502, got %#v", err)
	}
}

func TestForbidden(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":50013,"message":"Missing Permissions"}`))
		})
		_, err := c.CreateWebhook(context.Background(), "900", "x")
		var fe *ForbiddenError
		if !errors.As(err, &fe) || fe.Status != status {
			t.Fatalf("status %d: want ForbiddenError, got %#v", status, err)
		}
		noLeak(t, err)
	}
}

func TestNetworkErrorIsRetryableAndHidesToken(t *testing.T) {
	c, srv := newTestClient(t, func(http.ResponseWriter, *http.Request) {})
	srv.Close()
	_, err := c.ListChannels(context.Background(), testGuild)
	var re *RetryableError
	if !errors.As(err, &re) || re.Status != 0 {
		t.Fatalf("want network RetryableError, got %#v", err)
	}
	noLeak(t, err)
}
