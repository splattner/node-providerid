# node-providerid

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

Use Go 1.23 or newer:

```sh
go mod tidy
go test ./...
go build -o node-providerid .
```

Run the binary on a k3s server with access to its etcd client certificates.
Flags come **after** `get` or `set`.

## Install

Prebuilt binaries for Linux and macOS (amd64/arm64) are attached to each
[GitHub Release](https://github.com/splattner/node-providerid/releases):

```sh
curl -fsSLo node-providerid \
  https://github.com/splattner/node-providerid/releases/latest/download/node-providerid-linux-amd64
chmod +x node-providerid
```

## Read

```sh
sudo ./node-providerid get --node worker-1
```

Prints just the providerID followed by a newline. An unset/empty providerID
prints a blank line. A missing Node is an error (exit status 1).

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

## Tests

Validated in this workspace with Go 1.24.4: build, unit tests and the optional
integration test against disposable etcd 3.5.21 passed. The live k3s deployment
and its mutual-TLS connection were not available for testing. The fuzz seed
cases ran as part of unit tests; an extended fuzz campaign was not run.

Unit tests cover protobuf unknown-field preservation, JSON numeric precision,
missing/empty values, invalid storage, name mismatches, and backup exclusivity.
The fuzz target checks successful codec transformations round-trip.

An optional integration test verifies successful writes, stale-revision rejection,
lease preservation and deleted-key rejection against a **disposable** etcd instance:

```sh
ETCD_TEST_ENDPOINT=http://127.0.0.1:2379 go test -run TestEtcdCompareAndPut -v
```

The integration test uses a random key under `/node-providerid-test/`, removes
it afterward, and skips when the environment variable is absent. Its client is
for local test etcd without TLS; the production CLI requires HTTPS and mutual TLS.

## Contributing

`main` is protected: all changes land through pull requests that pass CI
(`go vet`, `go test -race`, build). Commit messages follow
[Conventional Commits](https://www.conventionalcommits.org/) —
[release-please](https://github.com/googleapis/release-please) uses them to
maintain `CHANGELOG.md` and open a release PR. Merging that PR tags the version
and publishes a GitHub Release; a workflow then builds and attaches the binaries.

## References

- [Kubernetes protobuf encoding](https://kubernetes.io/docs/reference/using-api/api-concepts/)
- [Node protobuf schema](https://github.com/kubernetes/api/blob/master/core/v1/generated.proto)
- [Kubernetes protobuf envelope schema](https://github.com/kubernetes/apimachinery/blob/master/pkg/runtime/generated.proto)
- [etcd transactions and revisions](https://etcd.io/docs/v3.5/learning/api/)
- [k3s etcd snapshots](https://docs.k3s.io/cli/etcd-snapshot)

## License

[MIT](LICENSE)
