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

The installer downloads Go from the official Go distribution endpoint and downloads Xray from the official XTLS GitHub release endpoint by default. When using a mirror or custom `XRAY_ZIP_URL`, set `XRAY_ZIP_SHA256` and verify the source is trusted.
