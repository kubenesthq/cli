# KubeNest CLI

`kubenest` installs and operates the **KubeNest Platform**: a pinned, tested
bundle of everything a Kubernetes cluster needs above the control plane —
ingress, certificates, storage, backup, upgrade orchestration and OS patching —
installed as one versioned unit onto Ubuntu hosts you supply.

Full documentation: [docs.kubenest.io/platform](https://docs.kubenest.io/platform/install)

## What it does

```bash
# Authenticate to your control plane once
kubenest login --control-plane https://api.your-domain.com

# Install the platform bundle onto your hosts, over SSH
kubenest platform install \
  --bundle 1.4 \
  --name prod-1 \
  --server 10.0.1.10 \
  --agent  10.0.1.11 \
  --agent  10.0.1.12 \
  --ha single-server \
  --profile observability \
  --ssh-user ubuntu \
  --ssh-key ~/.ssh/id_ed25519
```

Two properties the design guarantees, enforced by tests rather than promised:

- **Your SSH keys never leave your machine.** They come from `--ssh-key`,
  ssh-agent or `~/.ssh/config`; they are never uploaded to the control plane
  and never written to logs.
- **Acceptance checks converge, they don't snapshot.** Every check waits for
  its condition within a deadline from the bundle manifest and reports
  `pass`, `converging` or `fail` — a component that retries its way to healthy
  is a pass, not a false alarm.

## Install

Download a release binary — each one ships with SHA-256 checksums, a Sigstore
signature and SLSA build provenance. Verification instructions are on every
[release page](https://github.com/kubenesthq/cli/releases).

## Build from source

```bash
go build -o kubenest ./cmd/kubenest
go test ./...
```

## Status

The command surface (`login`, `platform install|uninstall|upgrade`, `backup`)
is final and documented; the installer implementation is landing. Skeleton
subcommands exit non-zero and say so — they never pretend success.

The pre-2026 application-layer commands (`apps`, `deploy`, `exec`, `logs`,
`registry`, `teams`) were removed: they targeted a control-plane API that no
longer exists. They remain in git history.
