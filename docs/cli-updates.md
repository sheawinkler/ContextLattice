# Managed CLI Updates

The native CLI checks and upgrades an existing **public, release-managed** install
through the existing release installer. It is not a separate package-manager
updater, checkout migration, or file-replacement engine.
Current version/lane comes from that install's `.contextlattice-release.json`,
not the CLI binary's embedded version or another product lane's version.

```sh
contextlattice update --check --install-dir /path/to/managed-install
contextlattice update --apply --install-dir /path/to/managed-install
# Explicit file-only qualification; does not upgrade the running runtime/CLI:
contextlattice update --apply --no-launch --skip-agent-tools --install-dir /path/to/managed-install
```

Exactly one of `--check` and `--apply` is required. `--pretty` indents JSON.
Root precedence is explicit `--install-dir`, `CONTEXTLATTICE_INSTALL_DIR`, then
the current directory itself, only when it is a managed install. No ancestor,
home-directory, executable-location, or repository-root guessing occurs. Use a
canonical directory path without symlink components. Unmanaged directories,
Git checkouts, inconsistent metadata, and paid-file/public-metadata conflicts
are refused.

## Scope

- `--check` only reads managed metadata and bounded public release metadata.
  It creates no staging directory, downloads no installer bundle, invokes no
  installer, and does not touch config, auth, profiles, or runtime.
- `--apply` on Linux/macOS downloads the existing portable Unix bootstrap
  archive, verifies it, and invokes its actual `ContextLattice-Install.sh`
  entrypoint with an argv list, never an evaluated shell string. The existing
  installer performs normal launch/source-bound readiness checks and global
  CLI/tool/hook/profile refresh. Only `--no-browser` is supplied by default.
  Its shipped dependencies (including Bash, Docker, Go, and launch-time Python)
  remain required; this updater adds no Python runtime of its own.
- `--no-launch` explicitly opts into file-only application. It skips runtime
  launch/readiness and, under existing installer semantics, global tool refresh.
  `--skip-agent-tools` independently opts out of global tool/hook/profile refresh
  while retaining normal runtime launch/readiness unless `--no-launch` is set.
- Windows is **check-only** here. Apply with the supported native MSI installer;
  this CLI does not implement an alternative Windows replacement engine.
- Paid installs are rejected **before any public release request**, for checks
  as well as apply. Use the matching authenticated download/update path for
  that channel. There is no downgrade flag, public fallback, or version
  comparison between paid installs and public releases.

## Integrity And Recovery

Only the official public GitHub repository's latest stable release is used.
Tags must be `vX.Y.Z` or `vX.Y.Z-public`; draft/prerelease metadata is refused.
The CLI requires unique official asset URLs, positive IDs, advertised SHA-256
digests and sizes, then verifies downloaded bytes against those digests/sizes.
The stable manifest must contain exactly the three known native installer
names, match the public lane/tag/ref, and bind the commit resolved from the
GitHub tag (including a bounded annotated-tag chain). Manifest artifact sizes
and digests must agree with GitHub assets. Releases lacking digest metadata
fail closed. This is HTTPS/GitHub metadata integrity, not an independent
publisher signature.

HTTP requests have timeouts and bounded responses/downloads. HTTPS redirects
are limited to GitHub and its release-asset host; no arbitrary mirror, token,
provider account, or URL override is supported. The outer archive is limited
to 256 MiB and 256 members; its six expected files are closed-name checked.
Both archive layers reject traversal, duplicate paths, links, special files,
and extended metadata, with expanded-byte/member limits. Embedded payload
checksums and release identities must agree before invocation.

Staging is private and beside the managed install, on its filesystem. The
installer receives that staging path as `TMPDIR`, not an implicit internal
temporary directory. A sibling `.<install-name>.update-lock` directory serializes
CLI apply operations; inspect an abandoned lock before removing it, and never
run a separate installer concurrently. The HTTP deadline does not kill an
in-progress installer transaction. Existing installer transaction, atomic
publication, and state-preservation semantics remain authoritative. The CLI
passes `--no-browser` by default; it does not run the operator or open a browser.
Explicit Docker, tool-home, Go toolchain, and Go cache environment selections
are preserved for the existing installer without forwarding the full process
environment or API credentials. The installer's existing lite/full selection
(`FULL_MODE`) remains authoritative.
Only declared installer-preserved state (`.env`, `.data`, `data`, `backups`)
is retained; preserve any other local modifications before applying.

Same/older versions are reported without downloading or applying the bundle.
Applying a newer target requires v5.2.0 or later, including file-only mode:
earlier installers do not support this updater's flags and source-bound
readiness contract. Such targets are refused before staging or installer
execution. Checks and same/older-version no-ops remain available.
Installer failures propagate as CLI failures, and a successful invocation is
followed by an installed-identity readback. Installer diagnostics may identify
a retained transaction requiring inspection. The CLI never claims automatic
rollback after runtime commit. Successful normal apply reports
`runtime_verified: true` with the existing installer's source-bound readiness
as its verifier, not an independent running-runtime audit. File-only apply
reports `runtime_verified: false` and `restart_required: true`; checks/no-ops
do not verify runtime and leave restart requirements unknown (`null`).

Global CLI refresh can warn and fail without failing the installer's already
verified runtime upgrade. That warning is relayed and reported as
`cli_refresh_status: failed`; inspect diagnostics rather than treating the
running CLI as updated. `skipped` identifies explicit opt-outs. Otherwise the
status is `delegated_unverified`: the installer returned success from its normal
refresh path, but returns no independent CLI binary identity receipt. The
running updater can finish from its old executable inode; successful runtime
readiness is not proof that the replacement CLI is current. Verify the
replacement CLI separately and follow the platform installer's recovery guidance.
