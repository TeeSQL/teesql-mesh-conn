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
  presents a data-sidecar-issued leaf certificate loaded from
  `/teesql-shared/`.

## Build

```bash
go mod tidy
go build -o teesql-mesh-conn ./...
go test ./...
docker build -t ghcr.io/teesql/teesql-mesh-conn:v0.1.2 .
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

- `TEESQL_TURN_PROVIDER`: `coturn` or `cloudflare-calls`. Defaults to
  `coturn` for backwards compatibility. `coturn` uses `TURN_HOST` plus
  `TURN_SHARED_SECRET` or `/run/secrets/turn` and computes the legacy
  coturn REST-HMAC username/password locally.
- `TEESQL_TURN_CREDENTIAL_URL`: Worker endpoint used when
  `TEESQL_TURN_PROVIDER=cloudflare-calls`. Defaults to
  `https://mesh-signal.teesql.com/turn-credentials`.
- `TEESQL_SIGNALING_P256_PRIVATE_KEY_HEX`: 32-byte hex P-256 private key
  used to sign mesh-signal requests. Required for Cloudflare Calls TURN
  and optional for legacy coturn deployments until the Worker requires
  signed publish/poll.
- `TEESQL_SENDER_APP_ID`: sender app id placed in
  `X-Teesql-Sender-AppId`. Required with
  `TEESQL_SIGNALING_P256_PRIVATE_KEY_HEX`.
- `TEESQL_CLUSTER`: cluster diamond address placed in `X-Teesql-Cluster`.
  Required with `TEESQL_SIGNALING_P256_PRIVATE_KEY_HEX`.
- `TEESQL_CLUSTER_CA_PEM_PATH`: path to the cluster CA certificate PEM.
  Defaults to `/teesql-shared/cluster-ca.pem`.
- `TEESQL_LEAF_CERT_PATH`: path to this member's mesh-conn leaf
  certificate PEM. Defaults to `/teesql-shared/mesh-conn-leaf.pem`.
- `TEESQL_LEAF_KEY_PATH`: path to this member's mesh-conn leaf private
  key PEM. Defaults to `/teesql-shared/mesh-conn-leaf.key`.

The data-sidecar owns the cluster CA private key material and never hands
it to mesh-conn. At boot, data-sidecar derives the cluster CA, writes the
trust anchor to `/teesql-shared/cluster-ca.pem`, issues this member's
mesh-conn QUIC leaf, and writes:

- `/teesql-shared/mesh-conn-leaf.pem` with mode `0644`
- `/teesql-shared/mesh-conn-leaf.key` with mode `0600`

mesh-conn only loads the leaf cert/key and the CA root. It does not sign
certificates and does not read a cluster CA private key.

### Cloudflare Calls TURN

Cloudflare Realtime TURN uses long-lived account-side TURN keys to issue
short-lived per-session credentials. Do not put the Cloudflare TURN key or
API token in mesh-conn CVMs. The TeeSQL deployment path is:

1. Create a Cloudflare Calls/Realtime TURN key in the Cloudflare dashboard
   or with `POST /accounts/{account_id}/calls/turn_keys`.
2. Store the TURN key id and token only in the `mesh-signal.teesql.com`
   Worker secrets.
3. Expose `GET /turn-credentials` from the Worker. The response must be:

   ```json
   {
     "iceServers": [{
       "urls": [
         "stun:stun.cloudflare.com:3478",
         "turn:turn.cloudflare.com:3478?transport=udp",
         "turn:turn.cloudflare.com:3478?transport=tcp",
         "turns:turn.cloudflare.com:5349?transport=tcp"
       ],
       "username": "<ephemeral>",
       "credential": "<ephemeral>"
     }],
     "expiresAt": 1770000000
   }
   ```

4. Set `TEESQL_TURN_PROVIDER=cloudflare-calls` in mesh-conn. On startup,
   mesh-conn fetches signed credentials from the Worker, refreshes them at
   50% of their TTL, and retries failures with exponential backoff. Refresh
   failures are logged as
   `metric turn_credentials_refresh_failures_total=<n> provider=cloudflare-calls`.

Signed Worker requests use:

- `X-Teesql-Sig-Version: v1`
- `X-Teesql-Sender-AppId`
- `X-Teesql-Cluster`
- `X-Teesql-Nonce`
- `X-Teesql-Timestamp`
- `X-Teesql-Sig`

The signature is ECDSA P-256 over
`SHA-256(sender_app_id || cluster || nonce || timestamp || method || path || SHA-256(body))`,
encoded as base64 raw `r || s`.

## Upstream Sync

Check upstream for changes roughly quarterly. Rebase only after rerunning
the reconnect and QUIC certificate-pinning tests in this fork.
