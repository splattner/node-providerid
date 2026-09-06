# Security Policy

## Supported versions

Only the latest release is supported. Fixes are shipped as a new release.

## Reporting a vulnerability

Please report suspected vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/splattner/node-providerid/security/advisories/new)
rather than opening a public issue.

Include, as far as you can:

- the version (`node-providerid --version`) and how it was built or obtained;
- a description of the issue and its impact;
- steps or a proof of concept to reproduce it.

You can expect an initial response within 7 days.

## Scope

`node-providerid` speaks to etcd over mutual TLS and can write to a cluster's
backing store. Reports of particular interest:

- ways the `set` path could corrupt data outside the `providerID` field, or
  write to an unintended key;
- flaws in the protobuf/JSON codec that let malformed stored objects round-trip
  incorrectly;
- weaknesses in TLS/certificate handling or the compare-and-swap guard.

Running the tool with incorrect flags, or the inherent risk of direct etcd
writes documented in the README, is not a vulnerability.

## Verifying release binaries

Release binaries have a SLSA build provenance attestation:

```sh
gh attestation verify node-providerid-linux-amd64 --repo splattner/node-providerid
```

and a `checksums.txt` alongside them:

```sh
sha256sum -c checksums.txt
```
