package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMeshConnReconnectsAfterPeerRestart(t *testing.T) {
	bin := buildMeshBinary(t)
	certPath, keyPath := writeTestCA(t, t.TempDir(), "cluster")
	signal := newTestSignalBroker(t)
	defer signal.Close()

	aPort := freeLocalPort(t)
	bPort := freeLocalPort(t)
	peers := []Peer{
		{ID: "a", Ports: []int{aPort}},
		{ID: "b", Ports: []int{bPort}},
	}

	a := startMeshProc(t, bin, "a", peers, signal.URL, certPath, keyPath)
	defer a.stop()
	b := startMeshProc(t, bin, "b", peers, signal.URL, certPath, keyPath)
	defer b.stop()

	if !a.waitForCount("link up", 1, 30*time.Second) {
		t.Fatalf("peer a never linked up:\n%s", a.logs())
	}
	if !b.waitForCount("link up", 1, 30*time.Second) {
		t.Fatalf("peer b never linked up:\n%s", b.logs())
	}

	a.stop()
	a = startMeshProc(t, bin, "a", peers, signal.URL, certPath, keyPath)
	defer a.stop()

	if !b.waitForCount("link up", 2, 30*time.Second) {
		t.Fatalf("peer b did not reconnect within 30s\na logs:\n%s\nb logs:\n%s", a.logs(), b.logs())
	}
	if !a.waitForCount("link up", 1, 30*time.Second) {
		t.Fatalf("restarted peer a never linked up:\n%s", a.logs())
	}
}

func TestMeshConnRejectsWrongClusterCA(t *testing.T) {
	bin := buildMeshBinary(t)
	aCertPath, aKeyPath := writeTestCA(t, t.TempDir(), "cluster-a")
	bCertPath, bKeyPath := writeTestCA(t, t.TempDir(), "cluster-b")
	signal := newTestSignalBroker(t)
	defer signal.Close()

	aPort := freeLocalPort(t)
	bPort := freeLocalPort(t)
	peers := []Peer{
		{ID: "a", Ports: []int{aPort}},
		{ID: "b", Ports: []int{bPort}},
	}

	a := startMeshProc(t, bin, "a", peers, signal.URL, aCertPath, aKeyPath)
	defer a.stop()
	b := startMeshProc(t, bin, "b", peers, signal.URL, bCertPath, bKeyPath)
	defer b.stop()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if a.logCount("link up") > 0 || b.logCount("link up") > 0 {
			t.Fatalf("peers linked up despite different cluster CAs\na logs:\n%s\nb logs:\n%s", a.logs(), b.logs())
		}
		combined := a.logs() + "\n" + b.logs()
		if strings.Contains(combined, "certificate") ||
			strings.Contains(combined, "CRYPTO_ERROR") ||
			strings.Contains(combined, "tls:") ||
			strings.Contains(combined, "quic accept: context deadline exceeded") ||
			strings.Contains(combined, "quic dial: context deadline exceeded") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("wrong-CA peers did not link, but no TLS rejection was observed\na logs:\n%s\nb logs:\n%s", a.logs(), b.logs())
}

func TestClientTLSRejectsWrongClusterCA(t *testing.T) {
	aCertPath, aKeyPath := writeTestCA(t, t.TempDir(), "cluster-a")
	bCertPath, bKeyPath := writeTestCA(t, t.TempDir(), "cluster-b")
	clientCfg := &Config{SelfID: "a", ClusterCAPEMPath: aCertPath, ClusterCAKeyPath: aKeyPath}
	serverCfg := &Config{SelfID: "b", ClusterCAPEMPath: bCertPath, ClusterCAKeyPath: bKeyPath}

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- tls.Server(serverConn, serverTLS(serverCfg)).Handshake()
	}()

	err := tls.Client(clientConn, clientTLS(clientCfg)).Handshake()
	if err == nil {
		t.Fatal("client accepted a server certificate signed by the wrong cluster CA")
	}
	<-serverErr
}

func TestDeleteSessionDrainsAndRemovesAuth(t *testing.T) {
	sess := &peerSession{authCh: make(chan [2]string, 1)}
	sess.authCh <- [2]string{"old", "auth"}

	sessionsMu.Lock()
	sessions["peer"] = sess
	sessionsMu.Unlock()

	deleteSession("peer")

	if got := currentSession("peer"); got != nil {
		t.Fatalf("session still present: %#v", got)
	}
	select {
	case v := <-sess.authCh:
		t.Fatalf("authCh was not drained: %#v", v)
	default:
	}
}

type testSignalBroker struct {
	*httptest.Server
	mu     sync.Mutex
	queues map[string][]Message
}

func newTestSignalBroker(t *testing.T) *testSignalBroker {
	t.Helper()
	b := &testSignalBroker{queues: map[string][]Message{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/publish", func(w http.ResponseWriter, r *http.Request) {
		to := r.URL.Query().Get("to")
		var msg Message
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b.mu.Lock()
		b.queues[to] = append(b.queues[to], msg)
		b.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/poll", func(w http.ResponseWriter, r *http.Request) {
		peer := r.URL.Query().Get("peer")
		b.mu.Lock()
		msgs := append([]Message(nil), b.queues[peer]...)
		b.queues[peer] = nil
		b.mu.Unlock()
		_ = json.NewEncoder(w).Encode(msgs)
	})
	b.Server = httptest.NewServer(mux)
	return b
}

type meshProc struct {
	cmd   *exec.Cmd
	done  chan struct{}
	lines chan string
	mu    sync.Mutex
	buf   bytes.Buffer
}

func startMeshProc(t *testing.T, bin, id string, peers []Peer, signalURL, caCertPath, caKeyPath string) *meshProc {
	t.Helper()
	peersJSON, err := json.Marshal(peers)
	if err != nil {
		t.Fatalf("marshal peers: %v", err)
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"PEER_ID="+id,
		"PEERS_JSON="+string(peersJSON),
		"SIGNALING_URL="+signalURL,
		"TEESQL_CLUSTER_CA_PEM_PATH="+caCertPath,
		"TEESQL_CLUSTER_CA_KEY_PATH="+caKeyPath,
		"MESH_CONN_QUIC_KEEPALIVE_SECONDS=1",
		"MESH_CONN_QUIC_MAX_IDLE_SECONDS=2",
		"MESH_CONN_QUIC_HANDSHAKE_SECONDS=5",
		"MESH_CONN_LOOPBACK_ONLY=1",
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	p := &meshProc{
		cmd:   cmd,
		done:  make(chan struct{}),
		lines: make(chan string, 512),
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mesh-conn %s: %v", id, err)
	}
	go p.capture(stdout)
	go p.capture(stderr)
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	return p
}

func (p *meshProc) capture(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		p.mu.Lock()
		p.buf.WriteString(line)
		p.buf.WriteByte('\n')
		p.mu.Unlock()
		select {
		case p.lines <- line:
		default:
		}
	}
}

func (p *meshProc) stop() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	select {
	case <-p.done:
		return
	default:
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func (p *meshProc) waitForCount(substr string, want int, timeout time.Duration) bool {
	deadline := time.After(timeout)
	if p.logCount(substr) >= want {
		return true
	}
	for {
		select {
		case <-deadline:
			return p.logCount(substr) >= want
		case <-p.lines:
			if p.logCount(substr) >= want {
				return true
			}
		case <-p.done:
			return p.logCount(substr) >= want
		}
	}
}

func (p *meshProc) logCount(substr string) int {
	return strings.Count(p.logs(), substr)
}

func (p *meshProc) logs() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String()
}

func buildMeshBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mesh-conn-testbin")
	cmd := exec.Command("go", "build", "-o", path, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build test binary: %v\n%s", err, out)
	}
	return path
}

func writeTestCA(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ca key: %v", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		t.Fatalf("generate ca serial: %v", err)
	}
	cert := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal ca key: %v", err)
	}
	certPath := filepath.Join(dir, name+".pem")
	keyPath := filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatalf("write ca cert: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatalf("write ca key: %v", err)
	}
	return certPath, keyPath
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 50; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve tcp port: %v", err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		u, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err == nil {
			_ = u.Close()
			return port
		}
	}
	t.Fatal("could not find a local TCP+UDP port")
	return 0
}

func TestDurationEnv(t *testing.T) {
	t.Setenv("MESH_CONN_QUIC_MAX_IDLE_SECONDS", "3")
	if got := durationEnv("MESH_CONN_QUIC_MAX_IDLE_SECONDS", time.Minute); got != 3*time.Second {
		t.Fatalf("durationEnv = %v", got)
	}
	t.Setenv("MESH_CONN_QUIC_MAX_IDLE_SECONDS", "bad")
	if got := durationEnv("MESH_CONN_QUIC_MAX_IDLE_SECONDS", time.Minute); got != time.Minute {
		t.Fatalf("invalid durationEnv = %v", got)
	}
}

func Example_env() {
	fmt.Println(defaultClusterCAPEMPath)
	fmt.Println(defaultClusterCAKeyPath)
	// Output:
	// /teesql-shared/cluster-ca.pem
	// /teesql-shared/cluster-ca.key
}
