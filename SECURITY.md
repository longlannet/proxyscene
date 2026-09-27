# Security Policy

## Supported Versions

Only the latest release and the latest revision on the default branch receive security fixes.

## Reporting a Vulnerability

Use GitHub Private Vulnerability Reporting instead of a public issue:

https://github.com/longlannet/proxyscene/security/advisories/new

Include the affected release or commit, operating system, installation method, reproduction steps,
expected and actual impact, and sanitized logs. Remove node URLs, subscription URLs, tokens,
credentials, private keys, `state.json`, generated Xray configuration, and all `*-proxy-journal.json`
files plus their backups before submitting. Global/OpenClaw journals can contain original administrator
configuration, including proxy values with embedded credentials.

## Release Integrity Model

The release workflow on the default branch uses GitHub Release assets plus SHA256 only. There are no detached publisher signature assets or signing secrets. This model starts with `v0.8.0`; the published `v0.7.1` predates it and remains a legacy mutable release that the installer intentionally rejects.

Before dispatching a release, administrators enable immutable releases in GitHub Settings and use an account with Administration read permission to confirm that the setting remains enabled. The workflow's `GITHUB_TOKEN` does not have Administration permission, so it cannot reliably read that repository setting. Release is initiated only with `workflow_dispatch` from `main`; operators provide a strict stable `vMAJOR.MINOR.PATCH` version and do not create or push the tag. The build job pins the selected current `main` commit and retains module verification, formatting, normal and race tests, static and vulnerability analysis, two byte-identical four-architecture builds, exact archive and checksum verification, and the disposable Debian 13 systemd install/upgrade canary. Only the publish job has `contents: write`. It confirms that `main` has not moved and that the target tag and Release do not exist, creates a draft, uploads the complete asset set, and only then publishes it as Latest.

A following read-only job requires the API to report a non-draft, non-prerelease immutable Latest Release. It confirms that the created tag points to the tested commit, compares the exact asset set and GitHub-computed SHA256 digests with the local release set, downloads every asset for a byte-for-byte comparison, and verifies the downloaded `checksums.txt`. There is no protected Environment, tag ruleset, bypass configuration, or repository-variable acknowledgement requirement for GitHub publication. Mirror automation is separately opt-in.

If draft creation, asset upload, or publication is interrupted, do not blindly rerun the whole workflow. First inspect the same-name tag, draft/Release, target commit, and asset set. Remove an unpublished residual draft/tag manually only after that review, or rerun only a pending read-only verification job. The workflow never automatically deletes release objects.

`checksums.txt` covers `install.sh`, every manager archive, every offline bundle, and the fixed Xray corresponding-source archive. SHA256 detects corruption and inconsistent files, but it is not an independent publisher signature: a compromise of the GitHub repository/control plane before immutable publication can replace both an asset and its checksum. Obtain a known checksum through an independent trusted channel when that threat is in scope.

Never pipe a mutable branch script into a root shell. Download `install.sh` and `checksums.txt` from
the same explicit release tag directly into a root-only staging directory, verify the `install.sh`
entry there, review that same root-owned file, and execute it with `PROXYSCENE_VERSION` set to that
tag. Do not verify a user-writable copy and then execute it as root. The bootstrap process and installer both require the GitHub
REST metadata to contain the exact expected `tag_name` and `immutable: true`. When `latest` is
requested, the installer resolves it once and then uses only that fixed immutable tag. A custom
`PROXYSCENE_BASE_URL` requires an explicit version tag, changes only the manager archive location,
and never changes the canonical GitHub Release location used for `checksums.txt`.

## Mirror Boundaries

`https://dl.ll.cd/proxyscene/<tag>/` serves exactly the same 11 assets as the immutable GitHub
Release. The mirror is a download location, not an independent trust root. The recommended mirror
installation and upgrade path downloads the complete fixed-version bundle, while release metadata
comes from `api.github.com` and `checksums.txt` comes from that exact tag's canonical GitHub Release.
The archive must pass that checksum before extraction. A checksum served by the mirror cannot
authenticate its own assets. If GitHub is inaccessible, mirror-only authentication is unavailable;
use a bundle and canonical metadata/checksum previously verified on a trusted connected machine.

The root publishes only `latest.json` for discovery, with `version`, `tag`, `base_url`, `commit`,
`release_id`, and `published_at`. Clients still select and verify a fixed tag; this mutable pointer
does not authorize installation. There is no root-level mutable `install.sh`. Any explicitly downloaded
mirror installer would still require validation against the canonical GitHub checksum, and both
`PROXYSCENE_VERSION` and `PROXYSCENE_BASE_URL` must be fixed before execution. Do not use a
curl-to-shell entrypoint. The installer and CLI do not acquire a new self-update or mirror trust mode.

Every official release includes both GitHub and `dl.ll.cd`. Before building or publishing to GitHub,
the Release workflow requires `PROXYSCENE_RELEASE_MIRROR_CONFIGURED=true`; a missing or disabled
configuration blocks the release instead of skipping the mirror. All build and test gates, immutable
GitHub publication, and the complete post-publication verification must succeed before the required
`mirror-publish.yml` job starts. The dedicated non-root receiver accepts only fixed-tag
`sync` and `promote` forced commands; it fetches and validates assets from GitHub itself, stages files
outside the public tree, and atomically publishes a complete version directory. After CI anonymously
compares all 11 public assets, promotion atomically updates `latest.json` and rejects downgrades.
An SSH publication credential does not authorize arbitrary uploaded files or shell commands.
Mirror failure leaves the overall publication incomplete and preserves the immutable GitHub Release.
The mirror workflow also accepts an explicit published tag for an independent retry after recovery.
See [mirror publishing](docs/mirror-publishing.md) for deployment and recovery.

## Installer Boundaries

The online manager archive is size-limited and must have exactly one matching entry in the fixed
release's `checksums.txt`. The extracted manager must be a regular, non-symlink ELF for the current
architecture. Metadata, download, and checksum failures stop the Release installation; there is no
implicit source fallback. Source compilation requires explicit `PROXYSCENE_BUILD_FROM_SOURCE=1`
and an `install.sh` physically located in the intended source checkout containing `go.mod`.

Xray is fixed at `v26.9.9`, an upstream prerelease selected because the latest
upstream stable still uses an affected toolchain. All four archives are checked
against official digests; the exact source and matching compiler are checked for
vulnerable imported packages before publishing. The repository stores an independently reviewed SHA256 for each
supported archive (`amd64`, `arm64`, `386`, and `armv7`); the installer and bundle builder do not
trust a checksum downloaded beside the Xray archive. A custom HTTPS mirror must serve identical
bytes or be paired with an explicit `XRAY_ZIP_SHA256`. Existing Xray files are replaced on normal
online installs; `SKIP_XRAY_INSTALL=1` accepts only a root-owned, non-writable, regular non-symlink
ELF for the current architecture. `xray-version.txt` claims `v26.9.9` only for the pinned official
digest path. A custom archive is marked `custom-sha256:<archive digest>` and a retained binary is
marked `existing-sha256:<binary digest>`, so an unverifiable upstream version is never invented.

Offline mode is never auto-detected. It requires an explicit `--offline`, a bundle whose tar was
verified against the release `checksums.txt` before extraction, and a complete internal
`bundle-manifest.sha256`. Every component is rechecked against that manifest before any component
is copied. The internal manifest detects post-extraction corruption; because it is inside the same
bundle, it does not replace verification of the outer tar against a trusted release checksum. The
bundle must be extracted through a root-owned directory chain that is not writable by group or
others; do not run it from a shared temporary or user-controlled directory.

The installer fixes its umask at `077` before creating anything; lock acquisition repeats that guard,
and a fresh CoreDir is created with an explicit `0700` mode. Configured destination paths must be
normalized absolute paths with no `..`, repeated slash, or
trailing slash. Existing path ancestors must be root-owned, non-writable by group/others, and must
not be symlinks. Replacement files are staged beside their destination and renamed into place.
Before package-manager or destination changes, the installer takes a kernel `flock` on the
root-protected `/run/proxyscene-install.lock`; `flock` is therefore a hard prerequisite. Before the
file transaction it then acquires `/run/proxyscene-host-ownership.lock`. For an existing CoreDir, it
also acquires `.state.lock` and hands all three open-file descriptions to the new manager across
`exec`. For a fresh CoreDir, the installer hands off the install and host locks without pre-creating a
lock file in an unclaimed directory; after validating the new directory marker, the manager creates
and acquires `.state.lock` while both outer locks remain held. The manager validates every inherited
descriptor against the exact lock inode, root-only mode, and held `flock`. Locks therefore always
follow install, host, state order and are released only after manager initialization. This also blocks
a pre-upgrade manager that knows only the state lock from entering during replacement.
If `/etc/proxyscene-host-ownership.json` exists, it must be a root-owned `0600` non-symlink regular
file containing exactly one strict version-1 record whose CoreDir, binary, main unit, and restore
unit match the requested installation; malformed or conflicting records abort before replacement.
For a pre-ownership upgrade, a legacy CoreDir marker can claim the historical default binary only
when both requested legacy unit files contain the exact ExecStart bindings for that CoreDir and
binary. A copied or stale marker by itself is insufficient.
The installer keeps per-file backups and rolls those files back after download, validation, or file
replacement failures. Once all required files are ready, it commits that file transaction before
manager initialization. An initialization failure therefore retains the validated binaries and data
instead of leaving partially created systemd units pointing at rolled-back or missing dependencies.
Package-manager changes and manager/systemd side effects are outside the file transaction; retry
initialization with `proxyscene install --skip-node` after correcting the reported cause.

Source builds use Go 1.27.1 or newer. The default version is checked against repository-reviewed
SHA256 values and exact sizes for linux/386, amd64, arm64, and armv6l. Go does not provide dependable
per-archive `.sha256` URLs. Any non-default `GO_VERSION` therefore requires an explicit
`GO_TARBALL_SHA256` and otherwise fails closed. Downloads, backups, the module cache, build cache,
and GOPATH stay below the root-only `/run/proxyscene-install-tmp/transaction.*` directory. Because
`/run` is commonly mounted `noexec`, a downloaded executable Go toolchain is instead extracted into
a hidden root-only directory directly below the validated CoreDir, whose filesystem must also run
the managed Xray binary. Both temporary locations are removed at commit or rollback; the fixed empty
transaction root is retained for reuse, and the installer never replaces `/usr/local/go`.

The installer accepts no positional node or subscription URL. It always initializes with
`install --skip-node`; enter secrets afterward through the interactive installer or the
`node add/import --stdin` commands so they do not appear in process arguments.

## Runtime Ownership Boundaries

The core directory is a dedicated root-owned, non-group-writable location with a trusted
`.managed-by-proxyscene` marker. `installation-ownership.json` binds that directory, the installed
manager binary, and both systemd unit locators. Store operations fail closed when those locators do
not match. Service names are restricted to the `proxyscene`, `proxyscene-*`, or `proxyscene@*`
namespace, and an existing unit without the proxyscene ownership marker is neither overwritten nor
removed. An existing manager binary is replaceable only when ownership is provable. A matching
`installation-ownership.json` is required for current installations and custom paths; for legacy
compatibility only the historical default `/usr/local/bin/proxyscene` may be claimed when the old
CoreDir marker and both requested legacy unit ExecStart bindings agree. The marker alone cannot
claim the default binary or any custom path.

All state-changing commands also hold a fixed host lock and validate
`/etc/proxyscene-host-ownership.json`. The record binds one CoreDir, manager binary, and unit locator
set to the host-wide `/etc` and per-user proxy resources. A second installation with different
locators fails closed instead of nesting journals. The record is released only after uninstall has
restored shared resources and removed both managed units successfully.

Global proxy files use a root-only, generation-numbered main/backup journal written before either
fixed system path is changed. It records the exact original existence, bytes, uid, gid, mode,
managed metadata and bytes, and prepared/active/restoring
phase. Cleanup restores only a target that still matches recorded managed bytes. It never deletes
either fixed path without a valid journal. If an operator changes a managed regular file, that exact
state is durably reclassified as the new original before the other target is restored, then ownership
is released without overwriting the operator's content.

Hermes uses a separate root-only, generation-numbered journal with a durable backup and one entry
per direct systemd drop-in. The journal is prepared before the write and released only after the
correct daemon reload and service restart. Missing ownership never authorizes deletion; an
operator-modified drop-in is preserved. OpenClaw similarly journals the exact JSON container and
original value before using compare-and-swap writes. `state.json.telegram_targets` is migration
evidence for legacy drop-ins only; legacy shared environment files are retained when exclusive
ownership cannot be proved.

Telegram injection is accepted only when the effective runtime can be bound to the managed value.
For Hermes, an effective `NO_PROXY`/`no_proxy` entry matching `api.telegram.org`, `*`, or a public
IPv4 address/CIDR that can match a runtime DoH fallback would override `TELEGRAM_PROXY`; such targets
are rejected. Unit, manager, `PassEnvironment`, `UnsetEnvironment`, and `ExecStart` semantics are
combined, and any effective `EnvironmentFile` is rejected because its contents cannot be proven from
the merged unit. Active systemd specifiers, `ExecStart` `$` expansion, effective `ExecCondition`,
`ExecStartPre`, or `ExecStartPost` hooks, `PAMName`, `DynamicUser`,
`ProtectHome`, and root/bind/image/extension/tmpfs/inaccessible file-namespace redirection are also
rejected because they make the modeled identity, environment, argv, or host file view ambiguous.
The service identity, `HERMES_HOME`, active profile, and gateway project root must be
bound before the profile, operator, project, and `/etc/hermes` dotenv files are checked. Those files
must not redeclare proxy, bypass, fallback-IP, home, or managed-directory routing variables. The
post-write check also requires the final `TELEGRAM_PROXY` to equal the managed URL.
Automatic management requires a single default profile, with no named profile directories or symlinks.
Hermes 0.21.5 can multiplex automatically and retires the explicit false setting, while secondary
profiles do not reliably inherit process-level Telegram secrets. Named active profiles and explicit
multiplex activation are therefore rejected, including during recovery. Bypass checks cover Unicode
whitespace, wildcard apexes, scheme-relative hosts, and IPv4 dotted netmasks/hostmasks. Dotenv key
checks cover the Python/Node whitespace rules and reject ambiguous route declarations.
Hermes installation and configuration directories are identified independently from the effective
service executable and the bound user's `HERMES_HOME`. Supported layouts are the existing
`<Hermes root>/hermes-agent` checkout and the official `/usr/local/lib/hermes-agent` system install.
The latter may serve a non-root user, but its project and virtual-environment directories must exist,
be root-owned and not be group/world writable, with no symlink directory components. Its optional
project `.env` uses the same root-owned, bounded, no-follow reader as managed configuration and the
same routing-key checks as user dotenv files. A missing project is an error, not an absent optional file.
System-install interpreter paths may contain leaf symlinks and uv directory symlinks. Resolution uses
descriptor-relative, no-follow component checks with a finite hop limit: every link, traversed directory,
and final executable must be root-owned, and directories/files cannot be group/world writable.
Noncanonical link targets and cycles fail closed. Project and configuration paths retain their separate
no-symlink rules. This retains the trust assumption of an
administrator-maintained official installation; it is not a general proof of arbitrary Python `.pth`
hooks or import finders. Runtime validation does not execute the target interpreter for discovery.
The current layout is revalidated for each operation; the ownership journal continues to track only
proxyscene's managed configuration and user identity.
The managed Hermes drop-in also sets `PYTHONSAFEPATH=1`, and the post-restart check requires that
exact final value. This prevents `python -m hermes_cli.main` from prepending `WorkingDirectory` to
the module search path; conflicting unit, manager, dotenv, or secret-source declarations fail closed.
`HERMES_S6_SUPERVISED_CHILD` is treated as a routing selector too because it changes active-profile
resolution before application dotenv loading. Hermes dotenv data is decoded only as valid UTF-8 or
BOM-tagged UTF-16; UTF-32, untagged NUL-padded data, invalid UTF-8/latin1 fallback, and a routing key
that Hermes' sanitizer can split out of a preceding value all fail closed. `PYTHONHOME`, `PYTHONPATH`,
`NODE_OPTIONS`, `NODE_PATH`, `LD_PRELOAD`, `LD_LIBRARY_PATH`, and `LD_AUDIT` are rejected for the
applicable runtime because they can change the code loaded after argv validation.
For OpenClaw, every effective `ExecStart` must directly invoke an absolute clean `node`/`nodejs`
executable, an absolute clean path ending in `/openclaw/dist/index.js`, and the immediate `gateway`
argument. Shell, `env`, `chroot`, and other wrappers are not accepted. The effective unit must also
declare the expected user `HOME` and no config-selection environment, `--profile`, `--dev`, or
`EnvironmentFile`. The default OpenClaw `.env` files and JSON
configuration are checked for path selectors, shell environment import, and `$include`. Account-level
Telegram `proxy` values are rejected because they override `channels.telegram.proxy` after account
configuration merging. If the canonical `~/.openclaw/openclaw.json` is absent while one of OpenClaw's
legacy default candidates is active, management is rejected because the journal does not span config
paths. These checks deliberately fail closed instead of claiming partial injection.
Hermes bridges top-level scalar values from both the active profile and managed `/etc/hermes/config.yaml`
into the process environment. Any managed routing key at that level is rejected, and the managed file
must be a root-owned, non-group/world-writable regular file reached without symlinks. `config.yaml`
secret sources are likewise rejected unless their enabled mapping can be proven not to inject any
managed routing variable after the dotenv and systemd checks have completed.

Current Dev backups and per-user Hermes/OpenClaw journal entries bind the username to its numeric uid,
primary gid, and normalized home. The identity is revalidated before user-home access, drop-in
changes, user daemon reloads, or service restarts. If an account is deleted and recreated under the
same name, or its gid/home changes, proxyscene fails closed, preserves the ownership record, does not
touch the new home, and does not restart the new account's same-named service.

Do not delete a journal or backup to bypass an identity-drift error. Restore the original identity and
retry, or manually audit the old home configuration, direct drop-ins, OpenClaw configuration, and all
related root-only backups before isolating a confirmed stale per-user record. Upgrades from v0.7.1
should disable an active Dev scene with the old binary first where possible. Legacy Dev backups and
legacy user Telegram targets do not contain a stable uid/gid/home binding and therefore require
manual review; they are never applied to or removed from a newly created same-named account.
