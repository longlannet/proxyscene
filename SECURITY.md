# Security Policy

## Supported Versions

Only the latest version on the default branch is actively reviewed for security fixes.

## Reporting a Vulnerability

Please report suspected vulnerabilities privately before opening a public issue.

Include as much detail as possible:

- Affected version or commit.
- Operating system and installation method.
- Steps to reproduce.
- Expected and actual impact.
- Relevant logs with secrets, node URLs, subscription URLs, tokens, and private keys removed.

## Sensitive Data Notice

Do not publish or attach the following data in issues, pull requests, screenshots, or logs:

- Xray node links such as VLESS, VMess, Trojan, or Shadowsocks URLs.
- Subscription URLs.
- Generated `state.json` or `config.json` files.
- Telegram bot tokens, service credentials, private keys, or access tokens.
- Local system user information beyond what is required to reproduce an issue.

## Installation Script Supply Chain

The installer downloads the prebuilt `proxyscene` manager binary from this project's GitHub Releases by default. Downloads are restricted to HTTPS (including redirects), size-limited, and verified against `checksums.txt` (SHA256). The installer embeds the release public key, so `checksums.txt.minisig` is verified with minisign **by default whenever minisign is installed** (best-effort: skipped only if minisign is absent, in which case a warning is printed and SHA256-only verification remains — note SHA256 alone does not protect against an untrusted mirror that controls both the binary and `checksums.txt`, so install minisign for a real guarantee). Setting `PROXYSCENE_MINISIGN_PUBKEY` makes signature verification **mandatory** (a missing minisign binary or a download/verify failure becomes a hard error). A missing or mismatched checksum is always a hard failure; only network-download failures fall back to compiling from source. Release artifacts are produced by the `Release` GitHub Actions workflow, which cross-compiles `linux/amd64,arm64,386,armv7` and minisign-signs `checksums.txt` plus each offline bundle; on the canonical repo the release fails closed if signatures are missing.

Release `checksums.txt` files are signed with this minisign public key:

```
RWSwCDZeUKUXxnGQfkQwePkJyg1uKh7LcKXgia4Lto4MeC6lKStdotYb
```

Verify a downloaded release with `minisign -Vm checksums.txt -x checksums.txt.minisig -P <pubkey>`, or pass `PROXYSCENE_MINISIGN_PUBKEY=<pubkey>` to the installer.

For installs in networks that block GitHub, the `Release` workflow also publishes self-contained offline bundles (`proxyscene_bundle_linux_<arch>.tar.gz`) plus a per-bundle `.minisig`. Each bundle is a single top-level directory containing `install.sh`, the manager binary, Xray, and geo data. The offline flow is "extract and run": `tar xzf … && cd … && sudo ./install.sh` installs fully offline (no network, no Go); the script auto-detects the sibling binaries.

A self-contained bundle cannot cryptographically verify itself (the script and the binaries are both inside it), so "extract and run" trusts the source of the tar. To get an integrity guarantee, verify the whole tar with the per-bundle `.minisig` and the published public key **before extracting** (`minisign -Vm <bundle>.tar.gz -x <bundle>.tar.gz.minisig -P <pubkey>`). Before installing, the in-bundle installer requires each component to be a regular file and rejects symlink members. Writes are confined to the configured paths because `PROXYSCENE_SWITCH_BIN` is validated (absolute, non-symlink, no whitespace) and `PROXYSCENE_MANAGER_DIR` is restricted to a dedicated directory under `/opt`, `/var/lib`, or `/var/opt`; the manager binary is written with `install -D`.

When compiling from source, Go is downloaded from the official Go distribution endpoint and verified against the official `.sha256` file. Xray is downloaded from the official XTLS GitHub release endpoint by default and verified against the official `.dgst` checksum file. When using a mirror or custom `XRAY_ZIP_URL`/`PROXYSCENE_BASE_URL`, set `XRAY_ZIP_SHA256` / `PROXYSCENE_MINISIGN_PUBKEY` explicitly and verify the source is trusted.
