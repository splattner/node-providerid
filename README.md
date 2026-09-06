# node-providerid

[![CI](https://github.com/splattner/node-providerid/actions/workflows/ci.yml/badge.svg)](https://github.com/splattner/node-providerid/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/splattner/node-providerid?sort=semver)](https://github.com/splattner/node-providerid/releases/latest)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A Go CLI that reads or changes a Kubernetes Node's `spec.providerID` directly
through the etcd v3 API. Defaults target embedded etcd on a k3s server.
It does not open or edit etcd database files and does not call the Kubernetes API.
It requires an actual etcd-backed cluster; SQLite/Kine databases are not supported.

## Why this tool exists

A Node's `spec.providerID` links the Kubernetes Node object to its machine in
the cloud provider. The cloud-controller-manager and CSI drivers use it to
attach volumes, manage load-balancer members, and delete Node objects for
machines that no longer exist.

Kubernetes treats `spec.providerID` as **immutable once set**. The API server
rejects any attempt to change a non-empty value, and the field cannot be
cleared through `kubectl edit`, `kubectl patch`, or a normal client. Yet it
does get set wrong in practice:

- a Node registered before the cloud provider was configured, so kubelet set a
  placeholder such as `k3s://<node>`;
- a migration between providers, or a change to the instance-ID scheme, that
  left the stored value pointing at nothing;
- a bad `--provider-id` / cloud-config value baked into a Node at first boot.

The usual remedy is to drain, delete, and re-register the Node so it gets a
fresh `providerID`. When that is not practical, the only remaining place to
correct the value is the datastore itself. This tool does exactly that edit,
against etcd, as narrowly and reversibly as possible: it patches only the
`providerID` field, guards the write with an etcd compare-and-swap on the
Node's revision, and records a per-key backup first.

## ⚠️ Warning and disclaimer

**`set` writes directly to your cluster's backing store. This is a dangerous,
last-resort operation.** Read this section before running it.

- **etcd is the single source of truth for the entire cluster.** A malformed
  write to a Node key can make that Node object unreadable to the API server,
  and a mistake in the key path or value can affect more than one object.
- **Direct writes bypass every Kubernetes safety layer:** schema validation,
  admission control, authorization, defaulting, and API audit logging. Nothing
  checks that the value you write is sane.
- **`get` is read-only and safe.** All of the risk is in `set`.
- **Encoding is reverse-engineered.** The tool re-encodes the stored protobuf
  (or JSON) Node object. It aims to copy every unrelated byte unchanged and
  refuses ambiguous input, but it is not the Kubernetes apiserver's own
  storage codec and is not covered by any compatibility guarantee.
- **The change may not stick.** kubelet or a cloud-controller can overwrite
  `providerID` again after you set it. Fix the source of the wrong value too.
- **A controller may act on the new value immediately** — for example, the
  cloud-controller-manager can delete a Node whose `providerID` no longer
  resolves to a live instance.

Before using `set`:

1. Take a full etcd snapshot (`sudo k3s etcd-snapshot save ...`), not just the
   per-key backup this tool writes.
2. Try the supported path first: drain, delete, and re-register the Node.
3. Run with `--dry-run`, then with `--expect` pinned to the current value.
4. Test on a non-production cluster if you can.

This software is provided "as is", without warranty of any kind, under the
terms of the [MIT License](LICENSE). You are solely responsible for any use of
it against your clusters and for any data loss or outage that results. It is
not affiliated with or endorsed by the Kubernetes, etcd, or k3s projects.

## Build

Use Go 1.25 or newer:

```sh
go mod tidy
go test ./...
go build -o node-providerid .
```

Run the binary on a k3s server with access to its etcd client certificates.
Flags come **after** the subcommand. `node-providerid --version` prints the
build version.

## Install

Prebuilt binaries for Linux and macOS (amd64/arm64) are attached to each
[GitHub Release](https://github.com/splattner/node-providerid/releases):

```sh
curl -fsSLo node-providerid \
  https://github.com/splattner/node-providerid/releases/latest/download/node-providerid-linux-amd64
chmod +x node-providerid
```

Each release also has a `checksums.txt` and a SLSA build provenance attestation:

```sh
curl -fsSLO https://github.com/splattner/node-providerid/releases/latest/download/checksums.txt
sha256sum -c checksums.txt --ignore-missing

gh attestation verify node-providerid-linux-amd64 --repo splattner/node-providerid
```

## Read

```sh
sudo ./node-providerid get --node worker-1
```

Prints just the providerID followed by a newline. An unset/empty providerID
prints a blank line. A missing Node is an error (exit status 1).

## List

```sh
sudo ./node-providerid list
```

Prints one `<node-name><TAB><providerID>` line per stored Node, sorted by name,
for a quick audit of every Node at once. It is read-only. A Node whose stored
object cannot be decoded is reported on stderr and makes the command exit 1,
but does not stop the listing.

## Change

Direct etcd writes bypass Kubernetes validation, admission, authorization and
API audit logging. This permits changes the Node API might reject. Use this as
an administrative repair tool and take a cluster snapshot first:

```sh
sudo k3s etcd-snapshot save --name before-providerid-change
```

Keep embedded etcd running while using this CLI; stopping k3s also stops its
embedded etcd. Check the kubelet/cloud-controller configuration that supplies
the providerID, since a controller can subsequently overwrite your change.

Preview:

```sh
sudo ./node-providerid set --node worker-1 \
  --provider-id 'aws:///eu-central-1a/i-0123456789abcdef0' \
  --dry-run
```

Write (replace the expected value with the value returned by `get`):

```sh
sudo ./node-providerid set --node worker-1 \
  --expect 'k3s://worker-1' \
  --provider-id 'aws:///eu-central-1a/i-0123456789abcdef0' \
  --backup ./worker-1-before.json
```

`--expect` is optional. Passing `--expect ''` requires the existing providerID
to be empty. To clear it, explicitly pass `--provider-id ''`.

Every actual change requires `--backup`. The file is created with mode 0600
and exclusive creation, contains the original complete etcd value as base64,
its revision, lease, key, cluster ID and old providerID, and is synced before
the transaction. It is a per-key recovery record, not a full etcd snapshot.
An existing file is never overwritten. A failed write can leave a backup;
use a new filename for subsequent attempts. A no-op or dry-run creates no file.

```json
{
  "key": "/registry/minions/worker-1",
  "node": "worker-1",
  "providerID": "k3s://worker-1",
  "value_base64": "azhzAAo...=",
  "mod_revision": 123456,
  "create_revision": 42,
  "lease": 0,
  "cluster_id": "14841639068965178418",
  "endpoints": ["https://127.0.0.1:2379"],
  "tool_version": "v1.0.0",
  "saved_at": "2026-01-02T15:04:05.999999999Z"
}
```

The write uses an etcd transaction comparing the Node's **modification revision**
against the revision just read. Any intervening update or deletion causes failure
without a write. There is no automatic conflict retry. On a timeout or transport
failure the result can be uncertain: read the value again before retrying.
Successful output reports the committed revision, not a guarantee that a
controller will retain the value afterward.

Verify both etcd and the Kubernetes API:

```sh
sudo ./node-providerid get --node worker-1
sudo k3s kubectl get node worker-1 -o jsonpath='{.spec.providerID}{"\n"}'
```

No apiserver restart is needed. The apiserver serves Nodes from a watch on
etcd, so it observes the new value on the next watch event; its watch cache
converges within a second or so. If the API still shows the old value after a
few seconds, a controller has written it back.

To reverse just this change, run `set` with the old providerID recorded in the
backup, `--expect` set to the new value, and a fresh backup path. This preserves
Node updates made since the first change. Do not blindly write the full backed-up
Node over a newer Node. Full cluster recovery follows the k3s snapshot procedure.

## Connection defaults

| Flag | Default |
| --- | --- |
| `--endpoints` | `https://127.0.0.1:2379` |
| `--cacert` | `/var/lib/rancher/k3s/server/tls/etcd/server-ca.crt` |
| `--cert` | `/var/lib/rancher/k3s/server/tls/etcd/server-client.crt` |
| `--key` | `/var/lib/rancher/k3s/server/tls/etcd/server-client.key` |
| `--prefix` | `/registry` |
| `--timeout` | `10s` per operation |

Override all certificate paths for a custom k3s data directory or external etcd.
Multiple HTTPS endpoints can be comma-separated. Server certificate verification
is always enabled. The endpoint must match the server certificate's SANs.
This CLI uses mutual TLS and does not implement etcd username/password login.

On an HA k3s cluster (three embedded-etcd servers), point `--endpoints` at any
one member — a write goes through Raft to a quorum before it commits, and the
compare-and-swap semantics are cluster-wide. Run the tool from one server;
there is no need to repeat it per member.

Kubernetes Node keys use the historical resource name `minions`, so the default
key is `/registry/minions/<node-name>`. `--prefix` changes the storage prefix.
The stored object must identify itself as `v1/Node` and its metadata name must
match `--node` before either reading the providerID or writing.

## Encoding and preservation

- Supports Kubernetes `k8s\x00` protobuf envelopes and plain JSON Node objects.
- Patches protobuf fields `Unknown.raw` (2), `Node.spec` (2), and
  `NodeSpec.providerID` (3). All other protobuf wire bytes are copied unchanged,
  including unknown fields, metadata and status. Container lengths are re-encoded.
- JSON uses raw messages to retain unknown fields and exact numeric values;
  whitespace and object property order may change.
- Does not modify stored `metadata.resourceVersion`: the API storage layer
  derives resource versions from etcd revisions. The etcd lease is preserved.
- Rejects encrypted, unsupported, malformed and ambiguous relevant protobuf
  fields. It does not decrypt Nodes or perform full Kubernetes schema validation.
- The supplied ID is used verbatim, including an explicitly empty ID. Choose
  a provider-specific value appropriate for your cloud-controller and CSI setup.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Success. For `set`, this includes a no-op (`--provider-id` already matches) and a `--dry-run`. |
| `1` | Any error: bad flags, Node not found, `--expect` mismatch, undecodable stored object, backup path exists, concurrent modification, or an etcd/TLS failure. `set` prints whether anything was written. |

`get` writes only the providerID to stdout; `list` writes `name<TAB>providerID`
lines to stdout. All diagnostics go to stderr.

## Tests

Unit tests cover protobuf unknown-field preservation, JSON numeric precision,
missing/empty values, invalid storage, name mismatches, and backup exclusivity.
The fuzz target checks that successful codec transformations round-trip.

Integration tests run against a **disposable** etcd instance and are skipped
unless `ETCD_TEST_ENDPOINT` is set. They cover conditional writes (success,
stale-revision rejection, lease preservation, deleted-key rejection) and the
`list` output. They use keys outside `/registry` and clean up after themselves,
over a plaintext connection — the production CLI still requires HTTPS and mutual
TLS.

```sh
# with a throwaway local etcd on 127.0.0.1:2379
ETCD_TEST_ENDPOINT=http://127.0.0.1:2379 go test ./... -v
```

CI runs `gofmt`, `go vet`, `go test -race` (with an etcd service container so
the integration tests execute), a build, and `govulncheck`.

## Contributing

`main` is protected: every change lands through a pull request that passes CI.
Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/) —
[release-please](https://github.com/googleapis/release-please) uses them to
maintain `CHANGELOG.md` and open a release PR. Merging that PR tags the version
and publishes a GitHub Release; the release workflow then cross-compiles the
binaries, writes `checksums.txt`, attaches a build provenance attestation, and
uploads everything to the release. [Renovate](https://docs.renovatebot.com/)
keeps dependencies and Actions current.

## References

- [Kubernetes protobuf encoding](https://kubernetes.io/docs/reference/using-api/api-concepts/)
- [Node protobuf schema](https://github.com/kubernetes/api/blob/master/core/v1/generated.proto)
- [Kubernetes protobuf envelope schema](https://github.com/kubernetes/apimachinery/blob/master/pkg/runtime/generated.proto)
- [etcd transactions and revisions](https://etcd.io/docs/v3.5/learning/api/)
- [k3s etcd snapshots](https://docs.k3s.io/cli/etcd-snapshot)

## License

[MIT](LICENSE)
