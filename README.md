# TeeSQL mesh-conn

TeeSQL mesh-conn is a maintained fork of dstack-tee's `mesh-conn`
binary from `dstack-examples`. TeeSQL uses it as the mesh transport
sidecar for QUIC streams over pion/ICE and TURN.

Forked from upstream commit:
`dstack-tee/dstack-examples@f36201fffc6a807dc13c7ee3cfea6e9056a09c88`

## Attribution

The upstream mesh-conn source is licensed under Apache-2.0 by dstack-tee.
This fork retains the upstream `LICENSE` and adds `NOTICE` attribution.

## TeeSQL patches

- Robustness: `runPeerLink` now deletes the active peer session after a
  failed or closed attempt (`main.go:360`), and the ICE state callback
  deletes it on disconnected, failed, or closed transitions
  (`main.go:1055`). Session deletion drains `authCh` and closes the ICE
  agent (`main.go:952`). Reconnect attempts also republish auth and
  candidates while the ICE attempt is live (`main.go:1082`), so retry
  ordering cannot strand one side waiting for a dropped pre-session auth.
- Security: QUIC no longer uses `InsecureSkipVerify`. The client verifies
  the server chain against the cluster CA (`main.go:787`), the server
  requires and verifies client certs (`main.go:802`), and each process
  signs an ephemeral QUIC leaf cert from the cluster CA key
  (`main.go:817`).

## Build

```bash
go mod tidy
go build -o teesql-mesh-conn ./...
go test ./...
docker build -t ghcr.io/teesql/teesql-mesh-conn:v0.1.0 .
```

## Configuration

The upstream environment variables remain in use:

- `PEER_ID`
- `PEERS_JSON`
- `SIGNALING_URL`
- `TURN_HOST`
- `TURN_SHARED_SECRET`
- `MESH_CONN_RELAY_ONLY`

TeeSQL adds:

- `TEESQL_CLUSTER_CA_PEM_PATH`: path to the cluster CA certificate PEM.
  Defaults to `/teesql-shared/cluster-ca.pem`.
- `TEESQL_CLUSTER_CA_KEY_PATH`: path to the cluster CA private key PEM.
  Defaults to `/teesql-shared/cluster-ca.key`.

For v0.1, mesh-conn signs an ephemeral per-member QUIC certificate at
boot using the cluster CA key. The intended production provisioning model
is that the data-sidecar holds the cluster CA seed, signs a per-member
certificate and key, and writes the material into `/teesql-shared/` for
mesh-conn to load during startup.

## Upstream Sync

Check upstream for changes roughly quarterly. Rebase only after rerunning
the reconnect and QUIC certificate-pinning tests in this fork.
