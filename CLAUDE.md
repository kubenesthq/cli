# CLAUDE.md

Guidance for Claude Code when working in this repository.

## What this is

The KubeNest **platform CLI**: `kubenest login`, `kubenest platform
install|uninstall|upgrade`, `kubenest backup ...`. It installs the pinned
platform bundle onto Ubuntu 24.04 hosts over SSH. The spec is
`kubenest-docs/content/platform/install.mdx` — read it before changing
behavior; the docs are the contract.

The pre-2026 app-layer commands (apps, deploy, exec, logs, registry, teams)
were **removed in 2026-08** (frozen scope, decision D1; they targeted a
backend API that no longer exists — teams/X-Team-UUID). Do not reintroduce or
extend them.

## Build and test

```bash
go build ./...
go test -race ./...
go vet ./...
gofmt -l .          # must print nothing
```

CI (`.github/workflows/ci.yml`) also cross-compiles for
linux/darwin/windows × amd64/arm64 with CGO disabled. Keep the build pure Go.

## Layout

```
cmd/kubenest/       thin main
pkg/cmd/            cobra command tree (root, login, platform, backup)
pkg/config/         ~/.kubenest/{config,credentials}.json — 0600 files in a
                    0700 dir, enforced on save; credentials.json holds the
                    knp_* CLI token keyed by control-plane URL (contract
                    v1.12.0 — the plaintext exists nowhere else)
pkg/api/            control-plane HTTP client (login, current user)
pkg/sshx/           SSH transport: --ssh-key / ssh-agent / ~/.ssh/config
pkg/converge/       convergence checks: pass / converging / fail — the ONLY
                    way the CLI waits on cluster state
pkg/manifest/       bundle manifest loader (limits.timeouts — every deadline)
pkg/k3s/            the ONE apply mechanism: manifests + HelmChart CRs into
                    k3s's auto-deploy dir over SSH; kubectl probe helpers
pkg/component/      platform component installers (S3 shape: render from
                    manifest → apply → converge.Wait → verify) — gatewayapi,
                    traefik (+ the kubenest-gateway defaults), certmanager
pkg/version/        version vars stamped via -ldflags by the release workflow
e2e/                real-host acceptance suite (build tag e2e) — needs a node
                    from `ephemeral-env.sh up --profile host`
```

## Invariants — tests enforce these; keep them true

1. **Key material never leaves the machine.** `pkg/sshx` key bytes are
   unprintable and unserializable (`redact.go`); `pkg/api` and `pkg/sshx`
   must never import each other (`arch_test.go`). The debug trace redacts
   `Authorization`. Never add a code path that logs or uploads a credential.
2. **Acceptance checks are convergence checks, never snapshots.** Anything
   that waits on cluster state goes through `pkg/converge` and reports
   `pass` / `converging` / `fail` — no other outcomes. `CrashLoopBackOff`
   and `Pending` are transient until the deadline.
3. **Deadlines come from the bundle manifest's `limits.timeouts`, never
   constants in code.** A missing timeout key is an error, not a default.
4. **Failure messages name a fix.** "traefik is Pending, no node matches its
   node selector" is a fix; "install failed" is not.
5. **Skeleton commands exit non-zero.** Unimplemented paths say so; they
   never pretend success.

## Release

Tag `v*` triggers `.github/workflows/build.yml`: tests, six binaries,
`checksums.txt`, keyless cosign signature over the checksums, SLSA provenance
per binary. Actions are pinned to commit SHAs — keep them pinned when
updating.
