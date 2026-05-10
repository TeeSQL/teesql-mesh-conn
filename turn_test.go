package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloudflareCallsCredentialsBuildICEConfig(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/turn-credentials" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		requireSignedHeaders(t, r, nil)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"iceServers": []map[string]any{
				{"urls": []string{"stun:stun.cloudflare.com:3478"}},
				{
					"urls": []string{
						"turn:turn.cloudflare.com:3478?transport=udp",
						"turn:turn.cloudflare.com:3478?transport=tcp",
						"turns:turn.cloudflare.com:5349?transport=tcp",
					},
					"username":   "ephemeral-user",
					"credential": "ephemeral-pass",
				},
			},
			"expiresAt": now.Add(time.Hour).Unix(),
		})
	}))
	defer server.Close()

	mgr := newTurnCredentialManager(server.URL+"/turn-credentials", testSigner(t), server.Client())
	mgr.now = func() time.Time { return now }

	urls, err := mgr.URIs(context.Background())
	if err != nil {
		t.Fatalf("URIs: %v", err)
	}
	if got, want := len(urls), 4; got != want {
		t.Fatalf("url count = %d, want %d", got, want)
	}
	if urls[0].Host != "stun.cloudflare.com" || urls[0].Username != "" {
		t.Fatalf("bad STUN URL: %#v", urls[0])
	}
	for _, u := range urls[1:] {
		if u.Host != "turn.cloudflare.com" {
			t.Fatalf("bad TURN host: %#v", u)
		}
		if u.Username != "ephemeral-user" || u.Password != "ephemeral-pass" {
			t.Fatalf("TURN credentials not attached: %#v", u)
		}
	}
}

func TestTurnCredentialRefreshAtHalfTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"iceServers": []map[string]any{{
				"urls":       []string{"turn:turn.cloudflare.com:3478?transport=udp"},
				"username":   "user-" + string(rune('0'+n)),
				"credential": "pass",
			}},
			"expiresAt": now.Add(2 * time.Hour).Unix(),
		})
	}))
	defer server.Close()

	mgr := newTurnCredentialManager(server.URL, testSigner(t), server.Client())
	mgr.now = func() time.Time { return now }
	if err := mgr.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls after initial refresh = %d", got)
	}
	mgr.mu.RLock()
	refreshAt := mgr.creds.RefreshAt
	expiresAt := mgr.creds.ExpiresAt
	mgr.mu.RUnlock()
	if refreshAt.Sub(now) != time.Hour {
		t.Fatalf("refreshAt = %v, want half TTL", refreshAt.Sub(now))
	}
	if expiresAt.Sub(now) != 2*time.Hour {
		t.Fatalf("expiresAt = %v, want 2h", expiresAt.Sub(now))
	}

	mgr.now = func() time.Time { return refreshAt.Add(time.Second) }
	mgr.mu.Lock()
	mgr.creds.ExpiresAt = mgr.now().Add(time.Hour)
	mgr.creds.RefreshAt = mgr.now().Add(-time.Second)
	mgr.mu.Unlock()
	if err := mgr.Refresh(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls after due refresh = %d, want 2", got)
	}
}

func TestTurnCredentialRetryBackoff(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "nope", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"iceServers": []map[string]any{{
				"urls":       []string{"turn:turn.cloudflare.com:3478?transport=udp"},
				"username":   "user",
				"credential": "pass",
			}},
			"expiresAt": now.Add(time.Hour).Unix(),
		})
	}))
	defer server.Close()

	var sleeps []time.Duration
	mgr := newTurnCredentialManager(server.URL, testSigner(t), server.Client())
	mgr.now = func() time.Time { return now }
	mgr.sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	if err := mgr.RefreshWithRetry(context.Background()); err != nil {
		t.Fatalf("RefreshWithRetry: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
	if len(sleeps) != 1 || sleeps[0] != time.Second {
		t.Fatalf("sleeps = %v, want [1s]", sleeps)
	}
	if got := atomic.LoadUint64(&mgr.failures); got != 1 {
		t.Fatalf("failures = %d, want 1", got)
	}
}

func TestSignedHeadersVerifyAgainstC2Digest(t *testing.T) {
	body := []byte(`{"from":"0xabc","type":"auth","data":"ufrag:pwd"}`)
	req, err := http.NewRequest(http.MethodPost, "https://mesh-signal.example/publish?to=b", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	signer := testSigner(t)
	if err := signer.Sign(req, body); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	requireSignedHeaders(t, req, body)
}

func testSigner(t *testing.T) *requestSigner {
	t.Helper()
	key, err := parseP256PrivateKeyHex("01")
	if err == nil || key != nil {
		t.Fatal("short private key unexpectedly parsed")
	}
	hexKey := strings.Repeat("0", 63) + "1"
	key, err = parseP256PrivateKeyHex(hexKey)
	if err != nil {
		t.Fatalf("parse test key: %v", err)
	}
	return &requestSigner{
		appID:   "0x" + strings.Repeat("11", 20),
		cluster: "0x" + strings.Repeat("22", 20),
		key:     key,
	}
}

func requireSignedHeaders(t *testing.T, r *http.Request, body []byte) {
	t.Helper()
	signer := testSigner(t)
	if r.Header.Get("X-Teesql-Sig-Version") != "v1" {
		t.Fatalf("missing signature version")
	}
	if r.Header.Get("X-Teesql-Sender-AppId") != signer.appID {
		t.Fatalf("bad app id header")
	}
	if r.Header.Get("X-Teesql-Cluster") != signer.cluster {
		t.Fatalf("bad cluster header")
	}
	nonce := r.Header.Get("X-Teesql-Nonce")
	if len(nonce) != 16 {
		t.Fatalf("nonce length = %d, want 16", len(nonce))
	}
	if _, err := hex.DecodeString(nonce); err != nil {
		t.Fatalf("nonce is not hex: %v", err)
	}
	ts, ok := new(big.Int).SetString(r.Header.Get("X-Teesql-Timestamp"), 10)
	if !ok {
		t.Fatalf("bad timestamp")
	}
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Teesql-Sig"))
	if err != nil {
		t.Fatalf("bad signature base64: %v", err)
	}
	if len(sig) != 64 {
		t.Fatalf("signature length = %d, want 64", len(sig))
	}
	digest := teesqlSignatureDigest(signer.appID, signer.cluster, nonce, ts.Int64(), r.Method, r.URL.Path, body)
	pubDER, err := x509.MarshalPKIXPublicKey(&signer.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubAny, err := x509.ParsePKIXPublicKey(pubDER)
	if err != nil {
		t.Fatal(err)
	}
	pub := pubAny.(*ecdsa.PublicKey)
	if !ecdsa.Verify(pub, digest, new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatalf("signature did not verify")
	}
}
