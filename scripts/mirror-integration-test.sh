#!/usr/bin/env bash
# Real SSH/HTTPS mirror acceptance in one disposable Docker container.
set -euo pipefail
[[ ${PROXYSCENE_MIRROR_CONTAINER_TEST-} == 1 ]] || {
  printf 'Set PROXYSCENE_MIRROR_CONTAINER_TEST=1 to run the isolated mirror integration test.\n' >&2
  exit 2
}
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
TAG=${PROXYSCENE_MIRROR_TEST_TAG:-v0.9.0}
[[ "$TAG" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ && ${#TAG} -le 128 ]]
EVIDENCE=${PROXYSCENE_MIRROR_TEST_EVIDENCE:-$(mktemp -d /tmp/proxyscene-mirror-integration.XXXXXXXX)}
mkdir -p -- "$EVIDENCE"
EVIDENCE=$(cd -- "$EVIDENCE" && pwd)
# Require a pre-existing image; the test does not create or pull any images.
IMAGE=$(docker image inspect --format '{{.Id}}' "${PROXYSCENE_MIRROR_TEST_IMAGE:-debian:13}")
docker image inspect --format '{"id":{{json .Id}},"repo_digests":{{json .RepoDigests}},"architecture":{{json .Architecture}},"os":{{json .Os}}}' "$IMAGE" > "$EVIDENCE/image.json"
TEST_ID=$(python3 -c 'import uuid; print(uuid.uuid4().hex)')
CONTAINER="proxyscene-mirror-test-$TEST_ID"
docker ps -aq --no-trunc | sort > "$EVIDENCE/container-ids-before.txt"
docker image ls -q --no-trunc | sort -u > "$EVIDENCE/image-ids-before.txt"
sha256sum "$ROOT/scripts/mirror-receiver.py" "$ROOT/scripts/mirror_release.py" \
  "$ROOT/deploy/nginx/proxyscene.conf" > "$EVIDENCE/source-before.sha256"
cleanup() {
  local result=$? cleanup_failed=0
  trap - EXIT INT TERM
  if docker container inspect "$CONTAINER" >/dev/null 2>&1; then
    if [[ $(docker inspect --format '{{index .Config.Labels "io.proxyscene.mirror-test"}}' "$CONTAINER") == "$TEST_ID" ]]; then
      mkdir -p -- "$EVIDENCE/container"
      docker cp "$CONTAINER:/evidence/." "$EVIDENCE/container/" >/dev/null 2>&1 || true
      docker logs "$CONTAINER" > "$EVIDENCE/container.log" 2>&1 || true
      docker rm -f "$CONTAINER" > "$EVIDENCE/container-removal.log" 2>&1 || cleanup_failed=1
    else
      printf 'Refusing cleanup: container identity changed.\n' >&2
      cleanup_failed=1
    fi
  fi
  docker ps -aq --no-trunc | sort > "$EVIDENCE/container-ids-after.txt"
  docker image ls -q --no-trunc | sort -u > "$EVIDENCE/image-ids-after.txt"
  if docker container inspect "$CONTAINER" >/dev/null 2>&1; then cleanup_failed=1; fi
  diff -u "$EVIDENCE/container-ids-before.txt" "$EVIDENCE/container-ids-after.txt" > "$EVIDENCE/container-ids.diff" || cleanup_failed=1
  diff -u "$EVIDENCE/image-ids-before.txt" "$EVIDENCE/image-ids-after.txt" > "$EVIDENCE/image-ids.diff" || cleanup_failed=1
  if ! sha256sum -c "$EVIDENCE/source-before.sha256" > "$EVIDENCE/source-after-check.txt"; then
    printf 'Tested source changed during integration.\n' >&2
    cleanup_failed=1
  fi
  printf 'test_exit=%s\ncleanup_failed=%s\ncontainer=%s\n' "$result" "$cleanup_failed" "$CONTAINER" > "$EVIDENCE/result.txt"
  if (( cleanup_failed )); then exit 1; fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
printf 'Evidence: %s\n' "$EVIDENCE"
docker run -d --init --name "$CONTAINER" --label "io.proxyscene.mirror-test=$TEST_ID" \
  --memory 1g --cpus 2 --pids-limit 256 "$IMAGE" sleep infinity > "$EVIDENCE/container-id.txt"
docker exec "$CONTAINER" mkdir -p /inputs /evidence
docker cp "$ROOT/scripts/mirror-receiver.py" "$CONTAINER:/inputs/mirror-receiver.py"
docker cp "$ROOT/scripts/mirror_release.py" "$CONTAINER:/inputs/mirror_release.py"
docker cp "$ROOT/deploy/nginx/proxyscene.conf" "$CONTAINER:/inputs/proxyscene.conf"
printf 'Preparing disposable SSH and HTTPS services...\n'
docker exec -i "$CONTAINER" bash -s > "$EVIDENCE/setup.log" 2>&1 <<'SETUP'
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d
chmod 755 /usr/sbin/policy-rc.d
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl nginx openssh-server openssh-client openssl python3
useradd --create-home --shell /bin/sh psmirror
passwd -l psmirror
install -d -m 0755 /usr/local/libexec/proxyscene-mirror /www /www/wwwroot /www/wwwroot/dl.ll.cd
install -m 0644 /inputs/mirror-receiver.py /inputs/mirror_release.py /usr/local/libexec/proxyscene-mirror/
install -d -o psmirror -g www-data -m 0755 /www/wwwroot/dl.ll.cd/proxyscene
install -d -o psmirror -g psmirror -m 0700 /var/lib/proxyscene-mirror /home/psmirror/.ssh
install -d -m 0700 /run/mirror-test
install -d -m 0755 /run/sshd
ssh-keygen -q -t ed25519 -N '' -f /run/mirror-test/client
{
  printf 'restrict,command="/usr/bin/python3 -I /usr/local/libexec/proxyscene-mirror/mirror-receiver.py" '
  cat /run/mirror-test/client.pub
} > /home/psmirror/.ssh/authorized_keys
chown psmirror:psmirror /home/psmirror/.ssh/authorized_keys
chmod 0600 /home/psmirror/.ssh/authorized_keys
ssh-keygen -A
awk '{print "[127.0.0.1]:2222 " $1 " " $2}' /etc/ssh/ssh_host_ed25519_key.pub > /run/mirror-test/known_hosts
cat > /run/mirror-test/sshd_config <<'SSHD'
Port 2222
ListenAddress 127.0.0.1
HostKey /etc/ssh/ssh_host_ed25519_key
PidFile /run/mirror-test/sshd.pid
UsePAM yes
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
AllowUsers psmirror
StrictModes yes
PermitUserEnvironment no
X11Forwarding no
AllowTcpForwarding no
AllowAgentForwarding no
PermitTunnel no
AuthorizedKeysFile .ssh/authorized_keys
LogLevel VERBOSE
SSHD
/usr/sbin/sshd -t -f /run/mirror-test/sshd_config
/usr/sbin/sshd -f /run/mirror-test/sshd_config -E /evidence/sshd.log
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -keyout /run/mirror-test/tls.key -out /usr/local/share/ca-certificates/proxyscene-mirror-test.crt \
  -subj '/CN=dl.ll.cd' -addext 'subjectAltName=DNS:dl.ll.cd' >/dev/null 2>&1
chmod 0600 /run/mirror-test/tls.key
update-ca-certificates
printf '127.0.0.1 dl.ll.cd\n' >> /etc/hosts
cat > /run/mirror-test/nginx.conf <<'NGINX'
user www-data;
worker_processes 1;
pid /run/mirror-test/nginx.pid;
error_log /evidence/nginx-error.log;
events { worker_connections 128; }
http {
    include /etc/nginx/mime.types;
    access_log /evidence/nginx-access.log;
    server {
        listen 443 ssl;
        server_name dl.ll.cd;
        root /www/wwwroot/dl.ll.cd;
        ssl_certificate /usr/local/share/ca-certificates/proxyscene-mirror-test.crt;
        ssl_certificate_key /run/mirror-test/tls.key;
        include /inputs/proxyscene.conf;
        location / { return 404; }
    }
}
NGINX
nginx -t -c /run/mirror-test/nginx.conf
nginx -c /run/mirror-test/nginx.conf
python3 --version
curl --version
nginx -v
/usr/sbin/sshd -V
SETUP
printf 'Running real anonymous GitHub sync, SSH restrictions and HTTPS verification...\n'
docker exec -i "$CONTAINER" python3 - "$TAG" > "$EVIDENCE/tests.log" 2>&1 <<'TESTS'
import fcntl
import hashlib
import json
import os
from pathlib import Path
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.request

tag = sys.argv[1]
root = Path('/www/wwwroot/dl.ll.cd/proxyscene')
evidence = Path('/evidence')
ssh = ['ssh', '-F', '/dev/null', '-i', '/run/mirror-test/client', '-p', '2222',
       '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', 'IdentitiesOnly=yes',
       '-o', 'IdentityAgent=none', '-o', 'UserKnownHostsFile=/run/mirror-test/known_hosts',
       '-o', 'GlobalKnownHostsFile=/dev/null', '-o', 'ConnectTimeout=5', 'psmirror@127.0.0.1']
checks = []
def command(name, request, succeeds=True):
    result = subprocess.run(ssh + [request], stdin=subprocess.DEVNULL, text=True,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=650)
    (evidence / (name + '.stdout')).write_text(result.stdout)
    (evidence / (name + '.stderr')).write_text(result.stderr)
    if (result.returncode == 0) != succeeds:
        raise AssertionError(f'{name}: exit={result.returncode}: {result.stderr}')
    if not succeeds and result.returncode == 255:
        raise AssertionError(f'{name}: SSH transport failed rather than receiver rejection')
    checks.append({'name': name, 'exit': result.returncode})
    print(f'PASS {name}', flush=True)
    return result

command('sync', 'proxyscene-mirror sync ' + tag)
assets = sorted((root / tag).iterdir())
assert len(assets) == 11
before = {p.name: [p.stat().st_ino, hashlib.sha256(p.read_bytes()).hexdigest()] for p in assets}
command('sync-retry', 'proxyscene-mirror sync ' + tag)
assert before == {p.name: [p.stat().st_ino, hashlib.sha256(p.read_bytes()).hexdigest()] for p in assets}
assert not list(Path('/var/lib/proxyscene-mirror').glob('stage-*'))
with open('/var/lib/proxyscene-mirror/.deploy.lock', 'rb') as lock:
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    locked = command('concurrent-sync-rejected', 'proxyscene-mirror sync ' + tag, False)
    assert 'another mirror deployment is active' in locked.stderr
for name, request in [('arbitrary-command', 'id'), ('shell-injection', 'proxyscene-mirror sync ' + tag + '; touch /tmp/forbidden'), ('empty-command', '')]:
    command(name, request, False)
assert not Path('/tmp/forbidden').exists()
command('promote', 'proxyscene-mirror promote ' + tag)
latest = (root / 'latest.json').read_bytes()
command('promote-retry', 'proxyscene-mirror promote ' + tag)
assert (root / 'latest.json').read_bytes() == latest
(evidence / 'latest.json').write_bytes(latest)
# The unmodified verifier uses the fixed dl.ll.cd HTTPS URL. Only this
# container trusts the temporary CA and resolves that name to local Nginx.
verify = subprocess.run(['/usr/bin/python3', '-I', '/usr/local/libexec/proxyscene-mirror/mirror_release.py',
    'verify', '--tag', tag, '--directory', str(root / tag), '--output', '/run/mirror-public', '--stable'],
    text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=650)
(evidence / 'https-verifier.stdout').write_text(verify.stdout)
(evidence / 'https-verifier.stderr').write_text(verify.stderr)
assert verify.returncode == 0, verify.stderr
(evidence / 'https-verifier.result.json').write_bytes(Path('/run/mirror-public.result.json').read_bytes())
checks.append({'name': 'unmodified-https-verifier', 'exit': verify.returncode})
print('PASS unmodified-https-verifier', flush=True)

responses = []
def request(path, method='GET'):
    req = urllib.request.Request('https://dl.ll.cd' + path, method=method,
                                 headers={'Accept-Encoding': 'gzip'})
    try:
        response = urllib.request.urlopen(req, context=ssl.create_default_context(), timeout=20)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        body = response.read()
        headers = {k.lower(): v for k, v in response.headers.items()}
        responses.append({'path': path, 'method': method, 'status': response.status, 'headers': headers,
                          'size': len(body), 'sha256': hashlib.sha256(body).hexdigest()})
        return response.status, headers, body

for asset in assets:
    path = '/proxyscene/' + tag + '/' + asset.name
    status, headers, body = request(path)
    assert status == 200 and body == asset.read_bytes(), path
    assert 'immutable' in headers.get('cache-control', '') and 'max-age=31536000' in headers['cache-control'], path
    assert headers.get('x-content-type-options') == 'nosniff' and 'content-encoding' not in headers, path
    status, headers, body = request(path, 'HEAD')
    assert status == 200 and not body and int(headers['content-length']) == asset.stat().st_size, path
status, headers, body = request('/proxyscene/latest.json')
assert status == 200 and body == latest
assert 'no-store' in headers.get('cache-control', '') and 'no-cache' in headers['cache-control']
assert headers.get('x-content-type-options') == 'nosniff' and 'content-encoding' not in headers
for path in ['/proxyscene', '/proxyscene/', '/proxyscene/install.sh', '/proxyscene/.secret',
             '/proxyscene/' + tag + '/', '/proxyscene/' + tag + '/.hidden',
             '/proxyscene/v01.2.3/install.sh', '/proxyscene/' + tag + '/unknown.txt',
             '/proxyscene/v999.999.999/install.sh']:
    status, headers, body = request(path)
    assert status == 404, (path, status)
    assert 'immutable' not in headers.get('cache-control', ''), path
for method in ['POST', 'PUT', 'DELETE', 'PATCH', 'OPTIONS']:
    for path in ['/proxyscene/latest.json', '/proxyscene/' + tag + '/install.sh']:
        status, headers, body = request(path, method)
        assert status in (403, 404, 405), (path, method, status)
# Verify existing forbidden objects cannot be served, then restore the exact
# public bytes before the final integrity and cleanup assertions.
script = root / tag / 'install.sh'
saved = Path('/run/mirror-test/install.sh.saved')
script.rename(saved)
try:
    script.symlink_to(saved)
    assert request('/proxyscene/' + tag + '/install.sh')[0] == 404
    script.unlink()
    script.mkdir()
    assert request('/proxyscene/' + tag + '/install.sh')[0] == 404
    script.rmdir()
finally:
    if script.is_symlink(): script.unlink()
    if script.is_dir(): script.rmdir()
    saved.rename(script)
assert before == {p.name: [p.stat().st_ino, hashlib.sha256(p.read_bytes()).hexdigest()] for p in assets}
assert (root / 'latest.json').read_bytes() == latest
assert not list(Path('/var/lib/proxyscene-mirror').glob('stage-*'))
(evidence / 'http-responses.json').write_text(json.dumps(responses, indent=2) + '\n')
(evidence / 'asset-identities.json').write_text(json.dumps(before, indent=2) + '\n')
(evidence / 'checks.json').write_text(json.dumps(checks, indent=2) + '\n')
print('PASS nginx-eleven-assets-headers-methods-paths-symlinks', flush=True)
print('MIRROR_INTEGRATION_OK', flush=True)
TESTS
cat "$EVIDENCE/tests.log"
printf 'MIRROR_INTEGRATION_COMPLETE evidence=%s\n' "$EVIDENCE"
