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

readme = Path(sys.argv[1]).read_text()
snippets = {}
for name in ('online', 'offline-download', 'offline-install'):
    match = re.search(r'<!-- bootstrap:' + name + r' -->\n```bash\n(.*?)\n```', readme, re.S)
    if not match:
        raise SystemExit(f'missing README snippet {name}')
    snippets[name] = match[1]

# Replacing these two host-specific values is the only change to the recipes.
# The commands, strict-mode scope, jq, awk, sha256sum, and tar remain real.
with tempfile.TemporaryDirectory(prefix='proxyscene-bootstrap-test-') as temporary:
    root = Path(temporary)
    bindir = root / 'bin'
    bindir.mkdir()
    (bindir / 'sudo').write_text('#!/bin/bash\nexec "$@"\n')
    (bindir / 'curl').write_text('''#!/bin/bash
set -euo pipefail
output=
while (( $# )); do
  case "$1" in -o) output=$2; shift 2 ;; *) url=$1; shift ;; esac
done
[[ -n "$output" ]]
name=${url##*/}
[[ "$url" != *api.github.com* ]] || name=release.json
[[ ${BOOTSTRAP_FAIL_DOWNLOAD-} != "$name" ]] || exit 22
cp -- "$BOOTSTRAP_FIXTURE/$name" "$output"
chmod 600 -- "$output"
''')
    for file in bindir.iterdir():
        file.chmod(0o700)

    def run_case(kind, fault):
        case = root / (kind + '-' + fault)
        case.mkdir()
        fixture = case / 'fixtures'
        fixture.mkdir()
        stages = case / 'stages'
        stages.mkdir()
        marker = case / 'executed'
        installer = ('#!/bin/bash\nset -euo pipefail\nprintf "%s\\n" "${PROXYSCENE_VERSION-}:$*" > ' + shlex.quote(str(marker)) + '\n').encode()
        (fixture / 'install.sh').write_bytes(installer)
        bundle = 'proxyscene_bundle_linux_amd64.tar.gz'
        with tarfile.open(fixture / bundle, 'w:gz') as archive:
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
        (fixture / 'release.json').write_text(metadata_text)
        asset = 'install.sh' if kind == 'online' else bundle
        digest = hashlib.sha256((fixture / asset).read_bytes()).hexdigest()
        line = digest + '  ' + asset + '\n'
        if fault == 'bad-sha': line = '0' * 64 + '  ' + asset + '\n'
        if fault == 'duplicate': line += line
        if fault == 'missing': line = digest + '  absent-file\n'
        (fixture / 'checksums.txt').write_text(line)
        snippet = snippets[kind]
        stage_prefix = str(stages / 'proxyscene-bootstrap')
        snippet = snippet.replace('/root/proxyscene-bootstrap', stage_prefix)
        snippet = snippet.replace('export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin', 'export PATH=' + shlex.quote(str(bindir) + ':/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'))
        snippet = snippet.replace('<release-tag>', 'v1.2.3')
        if kind == 'offline-install':
            stage = stages / 'proxyscene-bootstrap-v1.2.3-amd64.ABCDef12'
            stage.mkdir(mode=0o700)
            for file in fixture.iterdir():
                copied = stage / file.name
                copied.write_bytes(file.read_bytes())
                copied.chmod(0o600)
            if fault == 'world-writable': stage.chmod(0o777)
            if fault == 'symlink-asset':
                (stage / bundle).unlink()
                (stage / bundle).symlink_to(fixture / bundle)
            if fault == 'changed-after-download':
                with (stage / bundle).open('ab') as out: out.write(b'changed')
            snippet = snippet.replace('<staging-directory>', str(stage))
        env = dict(os.environ, PATH=str(bindir) + ':/usr/bin:/bin', BOOTSTRAP_FIXTURE=str(fixture))
        if fault == 'download-failure': env['BOOTSTRAP_FAIL_DOWNLOAD'] = asset
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
        if kind == 'offline-install' and not expect_success and list(stage.glob('extracted.*')):
            raise AssertionError(f'{kind}/{fault}: archive extracted before verification')
        print(f'PASS {kind}/{fault}')

    faults = ('valid', 'mutable', 'wrong-tag', 'malformed-json', 'multi-json', 'bad-sha', 'duplicate', 'missing')
    for kind in snippets:
        for fault in faults: run_case(kind, fault)
        if kind != 'offline-install': run_case(kind, 'download-failure')
    for fault in ('world-writable', 'symlink-asset', 'changed-after-download'):
        run_case('offline-install', fault)
print('README bootstrap regressions passed (29 cases)')
PY
