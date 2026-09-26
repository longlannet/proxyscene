#!/usr/bin/env bash
# Exercise the actual README recipes with synthetic downloads and an inert installer.
set -euo pipefail
if (( EUID != 0 )); then
  printf 'README ownership tests require root; run: sudo -- bash scripts/bootstrap-test.sh\n' >&2
  exit 1
fi
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
python3 - "$ROOT/README.md" <<'PY'
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tarfile
import tempfile
from urllib.parse import urlsplit

readme = Path(sys.argv[1]).read_text()
snippets = {}
for name in ('online', 'offline-download', 'offline-install'):
    match = re.search(r'<!-- bootstrap:' + name + r' -->\n```bash\n(.*?)\n```', readme, re.S)
    if not match:
        raise SystemExit(f'missing README snippet {name}')
    snippets[name] = match[1]

# Only the staging prefix, command PATH and documented placeholders are replaced.
# The commands, strict-mode scope, jq, awk, sha256sum, and tar remain real.
with tempfile.TemporaryDirectory(prefix='proxyscene-bootstrap-test-') as temporary:
    root = Path(temporary)
    bindir = root / 'bin'
    bindir.mkdir()
    (bindir / 'sudo').write_text('#!/bin/bash\nexec "$@"\n')
    (bindir / 'curl').write_text('''#!/usr/bin/python3
import os
from pathlib import Path
import shutil
import sys
from urllib.parse import urlsplit

args = iter(sys.argv[1:])
output = None
url = None
for arg in args:
    if arg == '-o': output = next(args)
    elif arg.startswith('https://'): url = arg
if not output or not url:
    sys.exit(64)
with open(os.environ['BOOTSTRAP_REQUESTS'], 'a') as log:
    log.write(url + '\\n')
if os.environ.get('BOOTSTRAP_FAIL_DOWNLOAD') == url:
    sys.exit(22)
parsed = urlsplit(url)
if parsed.scheme != 'https' or parsed.netloc not in ('api.github.com', 'github.com', 'dl.ll.cd') or parsed.query or parsed.fragment:
    sys.exit(64)
source = Path(os.environ['BOOTSTRAP_FIXTURE']) / parsed.netloc / parsed.path.lstrip('/')
shutil.copyfile(source, output)
os.chmod(output, 0o600)
''')
    for file in bindir.iterdir():
        file.chmod(0o700)

    api_url = 'https://api.github.com/repos/longlannet/proxyscene/releases/tags/v1.2.3'
    canonical_base = 'https://github.com/longlannet/proxyscene/releases/download/v1.2.3'
    mirror_base = 'https://dl.ll.cd/proxyscene/v1.2.3'
    case_count = 0

    def fixture_path(fixture, url):
        parsed = urlsplit(url)
        path = fixture / parsed.netloc / parsed.path.lstrip('/')
        path.parent.mkdir(parents=True, exist_ok=True)
        return path

    def run_case(kind, fault):
        global case_count
        case_count += 1
        case = root / (kind + '-' + fault)
        case.mkdir()
        fixture = case / 'fixtures'
        fixture.mkdir()
        stages = case / 'stages'
        stages.mkdir()
        marker = case / 'executed'
        request_log = case / 'requests.txt'
        installer = ('#!/bin/bash\nset -euo pipefail\nprintf "%s\\n" "${PROXYSCENE_VERSION-}:$*" > ' + shlex.quote(str(marker)) + '\n').encode()
        fixture_path(fixture, canonical_base + '/install.sh').write_bytes(installer)
        bundle = 'proxyscene_bundle_linux_amd64.tar.gz'
        with tarfile.open(fixture_path(fixture, canonical_base + '/' + bundle), 'w:gz') as archive:
            entry = tarfile.TarInfo('proxyscene_bundle_linux_amd64/install.sh')
            entry.size = len(installer)
            entry.mode = 0o700
            archive.addfile(entry, io.BytesIO(installer))
        metadata = {'tag_name': 'v1.2.3', 'immutable': True}
        if fault == 'mutable': metadata['immutable'] = False
        if fault == 'wrong-tag': metadata['tag_name'] = 'v9.9.9'
        metadata_text = json.dumps(metadata)
        if fault == 'malformed-json': metadata_text = '{broken'
        if fault == 'multi-json': metadata_text += '\n' + json.dumps(metadata)
        fixture_path(fixture, api_url).write_text(metadata_text)
        asset = 'install.sh' if kind == 'online' else bundle
        digest = hashlib.sha256(fixture_path(fixture, canonical_base + '/' + asset).read_bytes()).hexdigest()
        line = digest + '  ' + asset + '\n'
        if fault == 'bad-sha': line = '0' * 64 + '  ' + asset + '\n'
        if fault == 'duplicate': line += line
        if fault == 'missing': line = digest + '  absent-file\n'
        fixture_path(fixture, canonical_base + '/checksums.txt').write_text(line)
        # The mirror has independent files, including a checksum that clients
        # must never treat as authoritative. A forged checksum can match a
        # corrupt mirror archive while canonical GitHub still rejects it.
        for name in ('install.sh', bundle, 'checksums.txt'):
            fixture_path(fixture, mirror_base + '/' + name).write_bytes(
                fixture_path(fixture, canonical_base + '/' + name).read_bytes())
        if fault in ('mirror-corrupt', 'mirror-forged-checksum'):
            mirrored_asset = fixture_path(fixture, mirror_base + '/' + asset)
            mirrored_asset.write_bytes(mirrored_asset.read_bytes() + b'changed at mirror')
            if fault == 'mirror-forged-checksum':
                digest = hashlib.sha256(mirrored_asset.read_bytes()).hexdigest()
                fixture_path(fixture, mirror_base + '/checksums.txt').write_text(digest + '  ' + asset + '\n')
        snippet = snippets[kind]
        stage_prefix = str(stages / 'proxyscene-bootstrap')
        snippet = snippet.replace('/root/proxyscene-bootstrap', stage_prefix)
        snippet = snippet.replace('export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin', 'export PATH=' + shlex.quote(str(bindir) + ':/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'))
        snippet = snippet.replace('<release-tag>', 'v1.2.3')
        if kind == 'offline-install':
            stage = stages / 'proxyscene-bootstrap-v1.2.3-amd64.ABCDef12'
            stage.mkdir(mode=0o700)
            inputs = {
                'release.json': api_url,
                'checksums.txt': canonical_base + '/checksums.txt',
                bundle: mirror_base + '/' + bundle,
            }
            for name, url in inputs.items():
                copied = stage / name
                copied.write_bytes(fixture_path(fixture, url).read_bytes())
                copied.chmod(0o600)
            if fault == 'world-writable': stage.chmod(0o777)
            if fault == 'symlink-asset':
                (stage / bundle).unlink()
                (stage / bundle).symlink_to(fixture_path(fixture, mirror_base + '/' + bundle))
            if fault == 'changed-after-download':
                with (stage / bundle).open('ab') as out: out.write(b'changed')
            snippet = snippet.replace('<staging-directory>', str(stage))
        env = dict(os.environ, PATH=str(bindir) + ':/usr/bin:/bin', BOOTSTRAP_FIXTURE=str(fixture), BOOTSTRAP_REQUESTS=str(request_log))
        asset_url = (canonical_base if kind == 'online' else mirror_base) + '/' + asset
        if fault == 'download-failure': env['BOOTSTRAP_FAIL_DOWNLOAD'] = asset_url
        if fault == 'github-api-unavailable': env['BOOTSTRAP_FAIL_DOWNLOAD'] = api_url
        if fault == 'github-checksum-unavailable': env['BOOTSTRAP_FAIL_DOWNLOAD'] = canonical_base + '/checksums.txt'
        # No caller errexit: the README must protect itself in an ordinary shell.
        result = subprocess.run(['/bin/bash'], input=snippet, text=True, capture_output=True, env=env, timeout=20)
        expect_success = fault == 'valid'
        if (result.returncode == 0) != expect_success:
            raise AssertionError(f'{kind}/{fault}: unexpected rc={result.returncode}\n{result.stdout}\n{result.stderr}')
        expected_execution = expect_success and kind != 'offline-download'
        if marker.exists() != expected_execution:
            raise AssertionError(f'{kind}/{fault}: installer execution={marker.exists()}, expected {expected_execution}')
        if expected_execution:
            expected = 'v1.2.3:\n' if kind == 'online' else ':--offline\n'
            if marker.read_text() != expected:
                raise AssertionError(f'{kind}/{fault}: wrong installer arguments')
        if not expect_success and list(stages.glob('*/extracted.*')):
            raise AssertionError(f'{kind}/{fault}: archive extracted before verification')
        requested = request_log.read_text().splitlines() if request_log.exists() else []
        expected_requests = [] if kind == 'offline-install' else [api_url, canonical_base + '/checksums.txt', asset_url]
        if fault == 'github-api-unavailable': expected_requests = expected_requests[:1]
        if fault == 'github-checksum-unavailable': expected_requests = expected_requests[:2]
        if requested != expected_requests:
            raise AssertionError(f'{kind}/{fault}: wrong trust/download URLs: {requested}, expected {expected_requests}')
        print(f'PASS {kind}/{fault}')

    faults = ('valid', 'mutable', 'wrong-tag', 'malformed-json', 'multi-json', 'bad-sha', 'duplicate', 'missing')
    for kind in snippets:
        for fault in faults: run_case(kind, fault)
        if kind != 'offline-install': run_case(kind, 'download-failure')
    for fault in ('world-writable', 'symlink-asset', 'changed-after-download'):
        run_case('offline-install', fault)
    for kind in ('offline-download', 'offline-install'):
        for fault in ('mirror-corrupt', 'mirror-forged-checksum'):
            run_case(kind, fault)
    for kind in ('online', 'offline-download'):
        for fault in ('github-api-unavailable', 'github-checksum-unavailable'):
            run_case(kind, fault)
print(f'README bootstrap regressions passed ({case_count} cases; original 29 retained)')
PY
