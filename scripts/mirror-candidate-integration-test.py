#!/usr/bin/env python3
"""Real systemd, mirror-only candidate install and v0.11.0 self-update gate.

Requires PROXYSCENE_MIRROR_CONTAINER_TEST=1. The default Debian 13 image is
pinned by digest and is removed afterward if this run first downloaded it.
The candidate directory must contain all eleven real release assets. The old
bundle is bound to the independently reviewed immutable v0.11.0 SHA256; host
GitHub API access authenticates its release metadata before containers start.
Candidate metadata is an explicitly synthetic pre-publication identity, while
all served candidate bytes are the unmodified build outputs. The two disposable
containers use real HTTPS and systemd. After dependency preparation each has
only loopback networking; no host mounts, services, or ports are used.
"""
import argparse
import hashlib
import io
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import tarfile
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
_spec = importlib.util.spec_from_file_location("proxyscene_canary_mirror_release", ROOT / "scripts/mirror_release.py")
assert _spec is not None and _spec.loader is not None
mirror = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(mirror)
BASELINE_TAG = "v0.11.0"
BASELINE_COMMIT = "3f31f06c19fd40875c1a9738422d6df8b6636906"
BASELINE_SHA256 = "2301d6fad5db8bf33d10de1ad8aead70e790b8b32cacab94643c0bcb4389a195"
DEFAULT_IMAGE = "debian:13@sha256:fac46bff2e02f51425b6e33b0e1169f55dfb053d83511ca28aa50c09fd5ed7a4"


def sha(data):
    return hashlib.sha256(data).hexdigest()


def canonical(value):
    return (json.dumps(value, ensure_ascii=True, separators=(",", ":")) + "\n").encode()


def command(args, **kwargs):
    kwargs.setdefault("timeout", 360)
    return subprocess.run(args, check=True, **kwargs)


def components(blob):
    result = {}
    with tarfile.open(fileobj=io.BytesIO(blob), mode="r:gz") as stream:
        for entry in stream:
            parts = Path(entry.name).parts
            assert parts and parts[0] == "proxyscene_bundle_linux_amd64"
            assert not entry.name.startswith("/") and ".." not in parts
            assert entry.isdir() or entry.isfile()
            if entry.isfile():
                assert len(parts) == 2 and parts[1] not in result
                result[parts[1]] = stream.extractfile(entry).read()
    assert len(result) == 11 and {"proxyscene", "xray", "install.sh"} <= result.keys()
    return result


def inventory():
    containers = command(["docker", "ps", "-aq", "--no-trunc"], capture_output=True, text=True).stdout.splitlines()
    images = command(["docker", "image", "ls", "-q", "--no-trunc"], capture_output=True, text=True).stdout.splitlines()
    return {"containers": sorted(containers), "images": sorted(set(images))}


def interrupted(signum, _frame):
    raise RuntimeError("canary interrupted by signal " + str(signum))

SETUP = r'''
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d
chmod 755 /usr/sbin/policy-rc.d
apt-get update
apt-get install -y --no-install-recommends bash ca-certificates coreutils curl dbus iproute2 jq nginx openssl passwd procps python3 strace sudo systemd systemd-sysv tar unzip util-linux
apt-get clean
exec /sbin/init
'''

TLS = r'''
set -euo pipefail
install -d -m 0755 /www/wwwroot/dl.ll.cd /usr/local/libexec/proxyscene-mirror /run/mirror-only
cp -a /inputs/public/. /www/wwwroot/dl.ll.cd/
chmod -R a+rX /www/wwwroot/dl.ll.cd
install -m 0644 /inputs/bootstrap-install.sh /usr/local/libexec/proxyscene-mirror/bootstrap-install.sh
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -keyout /run/mirror-only/tls.key -out /usr/local/share/ca-certificates/mirror-only.crt -subj '/CN=dl.ll.cd' -addext 'subjectAltName=DNS:dl.ll.cd' >/dev/null 2>&1
chmod 600 /run/mirror-only/tls.key
update-ca-certificates
printf '127.0.0.1 dl.ll.cd\n' >> /etc/hosts
cp /inputs/candidate-latest.json /www/wwwroot/dl.ll.cd/proxyscene/latest.json
cat > /run/mirror-only/nginx.conf <<'NGINX'
user www-data;
worker_processes 1;
pid /run/mirror-only/nginx.pid;
error_log /evidence/nginx-error.log;
events { worker_connections 64; }
http {
    include /etc/nginx/mime.types;
    log_format proof '$remote_addr $host "$request" $status $body_bytes_sent';
    access_log /evidence/nginx-access.log proof;
    server {
        listen 127.0.0.1:443 ssl;
        server_name dl.ll.cd;
        root /www/wwwroot/dl.ll.cd;
        ssl_certificate /usr/local/share/ca-certificates/mirror-only.crt;
        ssl_certificate_key /run/mirror-only/tls.key;
        include /inputs/proxyscene.conf;
        location / { return 404; }
    }
}
NGINX
nginx -t -c /run/mirror-only/nginx.conf
nginx -c /run/mirror-only/nginx.conf
'''

TESTS = r'''
import fcntl,hashlib,json,os,socket,subprocess,tarfile
from pathlib import Path
E=Path('/evidence'); P=Path('/www/wwwroot/dl.ll.cd/proxyscene')
expected=json.loads(Path('/inputs/identities.json').read_text())
tag=expected['candidate']['tag']; scenario=Path('/inputs/scenario').read_text()
checks=[]
def run(name,args,succeeds=True,trace=False,data=None):
    actual=['strace','-f','-qq','-s','256','-e','trace=connect','-o',str(E/(name+'.trace'))]+args if trace else args
    result=subprocess.run(actual,input=data,text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=300)
    (E/(name+'.log')).write_text(result.stdout)
    assert (result.returncode==0)==succeeds,(name,result.returncode,result.stdout)
    checks.append({'name':name,'exit':result.returncode}); print('PASS '+name,flush=True)
    return result.stdout

def digest(path):return hashlib.sha256(Path(path).read_bytes()).hexdigest()
def snapshot():
    subprocess.run(['systemctl','is-active','--quiet','proxyscene.service'],check=True)
    pid=int(subprocess.check_output(['systemctl','show','--property=MainPID','--value','proxyscene.service'],text=True))
    assert pid>1 and digest('/proc/'+str(pid)+'/exe')==digest('/opt/proxyscene/xray')
    st=json.loads(Path('/opt/proxyscene/state.json').read_text())
    assert st['scene_enabled']['global'] is True and len(st['nodes'])==1
    assert st['subscriptions']==['https://subscription.example.invalid/mirror-canary']
    invocation=subprocess.check_output(['systemctl','show','--property=InvocationID','--value','proxyscene.service'],text=True).strip()
    assert len(invocation)==32
    return {'pid':pid,'invocation_id':invocation,'state':st,'config_sha256':digest('/opt/proxyscene/config.json'),
            'manager_sha256':digest('/usr/local/bin/proxyscene'),
            'ownership_sha256':digest('/etc/proxyscene-host-ownership.json'),
            'global_profile_sha256':digest('/etc/profile.d/proxyscene-global-proxy.sh')}

def identity(which):
    target=expected[which]
    assert digest('/usr/local/bin/proxyscene')==target['manager_sha256']
    assert digest('/opt/proxyscene/xray')==target['xray_sha256']
    version=run(which+'-version',['/usr/local/bin/proxyscene','version'])
    assert version.strip()=='proxyscene '+target['tag'][1:]+' ('+target['commit'][:12]+')',version

def preserved(before,after,exact=False):
    if exact:assert before==after,(before,after); return
    assert after['config_sha256']==before['config_sha256']
    assert after['ownership_sha256']==before['ownership_sha256']
    assert after['global_profile_sha256']==before['global_profile_sha256']
    for field in ('nodes','subscriptions','default_node_id','scene_nodes','scene_enabled','runtime_config'):
        assert after['state'].get(field)==before['state'].get(field),field

interfaces=subprocess.check_output(['ip','-j','link'],text=True)
(E/'links.json').write_text(interfaces)
assert {entry['ifname'] for entry in json.loads(interfaces)}=={'lo'}
assert not subprocess.check_output(['ip','route','show'],text=True).strip()
for host in ('140.82.114.4','1.1.1.1'):
    try: socket.create_connection((host,443),timeout=2)
    except OSError as error: (E/('blocked-'+host+'.log')).write_text(str(error)+'\n')
    else: raise AssertionError('Internet unexpectedly reachable: '+host)
checks.append({'name':'github-and-public-internet-unreachable','exit':0})
assert not Path('/usr/local/bin/proxyscene').exists()
run('fetch-fixed-bootstrap',['curl','-q','--fail','--silent','--show-error','--proto','=https','--max-redirs','0','--output','/run/mirror-only/bootstrap.sh','https://dl.ll.cd/proxyscene/install.sh'],trace=True)
assert Path('/run/mirror-only/bootstrap.sh').read_bytes()==Path('/inputs/bootstrap-install.sh').read_bytes()
if scenario=='fresh':
    run('readme-fresh-candidate-install',['bash','/inputs/readme-entry.sh'],trace=True)
    identity('candidate')
    run('same-version-bootstrap-repeat',['bash','/run/mirror-only/bootstrap.sh'],trace=True)
    identity('candidate')
    run('same-version-update-check',['/usr/local/bin/proxyscene','update','--check'],trace=True)
else:
    directory=Path('/opt/canary-baseline'); directory.mkdir(mode=0o700)
    with tarfile.open('/inputs/baseline.tar.gz','r:gz') as stream:
        for entry in stream:
            assert entry.isdir() or entry.isfile()
            assert not entry.name.startswith('/') and '..' not in Path(entry.name).parts
        stream.extractall(directory,filter='data')
    run('official-v0110-offline-install',['bash',str(directory/'proxyscene_bundle_linux_amd64/install.sh'),'--offline'],trace=True)
    identity('baseline')
    node='vless://11111111-1111-1111-1111-111111111111@127.0.0.1:9?encryption=none&security=none&type=tcp#mirror-canary\n'
    run('add-loopback-node',['/usr/local/bin/proxyscene','node','add','--stdin'],data=node)
    # Seed a saved subscription fixture, without permitting SSRF to the local
    # mirror or making any provider network request in this isolated canary.
    statepath=Path('/opt/proxyscene/state.json'); state=json.loads(statepath.read_text())
    state['subscriptions']=['https://subscription.example.invalid/mirror-canary']
    state['nodes'][0]['subscription_ids']=['sub-'+hashlib.sha256(state['subscriptions'][0].encode()).hexdigest()]
    state['nodes'][0]['subscription_managed']=True
    statepath.write_text(json.dumps(state)+'\n')
    run('activate-real-xray',['/usr/local/bin/proxyscene','global','on'])
    before=snapshot(); (E/'before-update.json').write_text(json.dumps(before,indent=2)+'\n')
    check=run('new-version-check',['/usr/local/bin/proxyscene','update','--check'],trace=True)
    assert tag in check and 'dl.ll.cd' in check
    archive=P/tag/'proxyscene_bundle_linux_amd64.tar.gz'
    blob=archive.read_bytes(); archive.write_bytes(bytes([blob[0]^1])+blob[1:])
    for name,args in [('corrupt-update-rejected',['/usr/local/bin/proxyscene','update','--yes']),
                      ('corrupt-bootstrap-rejected',['bash','/run/mirror-only/bootstrap.sh'])]:
        run(name,args,succeeds=False,trace=True); preserved(before,snapshot(),exact=True)
    archive.write_bytes(blob)
    # Hold the production flock in a separate file description. Both entry
    # points must reject instead of replacing bytes or restarting the core.
    with open('/run/proxyscene-install.lock','rb') as lock:
        fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        for name,args in [('concurrent-update-rejected',['/usr/local/bin/proxyscene','update','--yes']),
                          ('concurrent-bootstrap-rejected',['bash','/run/mirror-only/bootstrap.sh'])]:
            output=run(name,args,succeeds=False,trace=True)
            assert '另一个' in output or '锁' in output,output
            preserved(before,snapshot(),exact=True)
    run('default-running-self-update',['/usr/local/bin/proxyscene','update','--yes'],trace=True)
    identity('candidate'); after=snapshot(); preserved(before,after)
    (E/'after-update.json').write_text(json.dumps(after,indent=2)+'\n')
    run('updated-current-check',['/usr/local/bin/proxyscene','update','--check'],trace=True)
    run('candidate-node-probe',['/usr/local/bin/proxyscene','node','test'])
    probed=snapshot(); preserved(after,probed)
    assert (probed['pid'],probed['invocation_id'])==(after['pid'],after['invocation_id']),'node probe restarted the active production core'
    results=probed['state']['speed_results']; assert len(results)==1,results
    result=results[probed['state']['nodes'][0]['id']]
    assert result['success'] is False and result['error']=='节点 HTTPS 代理请求失败或超时（连接、认证或证书验证未通过）',result
    (E/'after-node-probe.json').write_text(json.dumps(probed,indent=2)+'\n')
    # A lower stable index must not roll back installed manager or runtime.
    (P/'latest.json').write_bytes(Path('/inputs/baseline-latest.json').read_bytes())
    run('bootstrap-downgrade-rejected',['bash','/run/mirror-only/bootstrap.sh'],succeeds=False,trace=True)
    run('updater-downgrade-skipped',['/usr/local/bin/proxyscene','update','--yes'],trace=True)
    preserved(probed,snapshot(),exact=True)
    pids=subprocess.check_output(['pgrep','-x','xray'],text=True).split()
    assert pids==[str(probed['pid'])],pids
for directory,pattern in [('/var/lib','proxyscene-bootstrap.*'),('/root','proxyscene-bootstrap.*'),
                          ('/opt/proxyscene','.proxyscene-update-*'),('/tmp','proxyscene-node-probe-*'),('/run/proxyscene-install-tmp','*')]:
    assert not list(Path(directory).glob(pattern)),(directory,pattern)
connections=[]
for path in sorted(E.glob('*.trace')):
    for line in path.read_text().splitlines():
        if 'AF_INET' in line:
            assert 'sin_port=htons(443)' in line and 'inet_addr("127.0.0.1")' in line,(path,line)
            connections.append({'trace':path.name,'connect':line})
assert connections
(E/'network-connections.json').write_text(json.dumps(connections,indent=2)+'\n')
logs=(E/'nginx-access.log').read_text().splitlines(); assert logs
for line in logs:
    assert line.startswith('127.0.0.1 dl.ll.cd "GET /proxyscene/'),line
    assert '" 200 ' in line,line
(E/'checks.json').write_text(json.dumps(checks,indent=2)+'\n')
print('MIRROR_ONLY_CANDIDATE_'+scenario.upper()+'_OK',flush=True)
'''


def run_scenario(scenario, image_id, inputs, evidence):
    evidence.mkdir()
    (inputs / 'scenario').write_text(scenario)
    token = uuid.uuid4().hex
    name = 'proxyscene-mirror-candidate-' + token
    targets = {'container': name, 'label': token, 'host_bind_mounts': [], 'published_ports': [], 'new_images': []}
    (evidence / 'cleanup-targets.json').write_bytes(canonical(targets))
    passed = False
    try:
        command(['docker', 'run', '-d', '--name', name, '--label', 'io.proxyscene.mirror-candidate=' + token,
                 '--privileged', '--cgroupns', 'private', '--stop-signal', 'SIGRTMIN+3',
                 '--tmpfs', '/run:rw,nosuid,nodev,noexec,mode=0755',
                 '--tmpfs', '/run/lock:rw,nosuid,nodev,noexec,mode=0755',
                 '--tmpfs', '/tmp:rw,nosuid,nodev,mode=1777', image_id, 'bash', '-ceu', SETUP],
                stdout=subprocess.DEVNULL)
        deadline = time.monotonic() + 240
        while time.monotonic() < deadline:
            state = subprocess.run(['docker', 'exec', name, 'systemctl', 'is-system-running'],
                                   capture_output=True, text=True, timeout=15).stdout.strip()
            if state in ('running', 'degraded'):
                break
            time.sleep(1)
        else:
            raise RuntimeError('systemd startup deadline exceeded')
        command(['docker', 'exec', name, 'mkdir', '-p', '/inputs', '/evidence'])
        command(['docker', 'cp', str(inputs) + '/.', name + ':/inputs/'])
        with (evidence / 'tls-setup.log').open('wb') as log:
            command(['docker', 'exec', '-i', name, 'bash', '-s'], input=TLS.encode(), stdout=log, stderr=subprocess.STDOUT)
        command(['docker', 'network', 'disconnect', 'bridge', name])
        networks = json.loads(command(['docker', 'inspect', '--format', '{{json .NetworkSettings.Networks}}', name],
                                     capture_output=True, text=True).stdout)
        assert networks == {}, networks
        command(['docker', 'exec', name, 'python3', '-c',
                 "import socket; f=open('/etc/hosts','a'); f.write('127.0.0.1 '+socket.gethostname()+'\\n'); f.close()"])
        (evidence / 'isolated-networks.json').write_bytes(canonical(networks))
        with (evidence / 'tests.log').open('wb') as log:
            command(['docker', 'exec', '-i', name, 'python3', '-'], input=TESTS.encode(), stdout=log,
                    stderr=subprocess.STDOUT, timeout=900)
        passed = True
    finally:
        found = subprocess.run(['docker', 'inspect', name], capture_output=True, text=True, timeout=30)
        if found.returncode == 0:
            inspected = json.loads(found.stdout)[0]
            assert inspected['Config']['Labels'].get('io.proxyscene.mirror-candidate') == token
            collection_errors = []
            try:
                for filename, argv, timeout in [
                    ('container.log', ['docker', 'logs', name], 30),
                    ('evidence-copy.log', ['docker', 'cp', name + ':/evidence/.', str(evidence)], 60),
                    ('journal.log', ['docker', 'exec', name, 'journalctl', '--no-pager', '-u', 'proxyscene.service',
                                     '-u', 'proxyscene-restore.service'], 30),
                ]:
                    try:
                        with (evidence / filename).open('wb') as log:
                            subprocess.run(argv, stdout=log, stderr=subprocess.STDOUT, timeout=timeout, check=True)
                    except (OSError, subprocess.SubprocessError) as error:
                        collection_errors.append(filename + ': ' + str(error))
            finally:
                # Evidence collection must never prevent deleting our identified
                # privileged container, including when Docker exec times out.
                command(['docker', 'rm', '-f', name], stdout=subprocess.DEVNULL)
            if collection_errors:
                (evidence / 'collection-errors.json').write_bytes(canonical(collection_errors))
                passed = False
        absent = subprocess.run(['docker', 'inspect', name], capture_output=True, timeout=30).returncode != 0
        (evidence / 'result.json').write_bytes(canonical({'passed': passed, 'container_absent': absent,
                                                         'real_systemd': True, 'scenario': scenario}))
        assert absent, 'canary container cleanup failed'
    assert passed

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--candidate-dir', type=Path, required=True)
    parser.add_argument('--candidate-tag', required=True)
    parser.add_argument('--candidate-commit', required=True)
    parser.add_argument('--baseline-bundle', type=Path, required=True)
    parser.add_argument('--baseline-record', type=Path)
    parser.add_argument('--evidence', type=Path)
    parser.add_argument('--image', default=DEFAULT_IMAGE)
    args = parser.parse_args()
    if os.environ.get('PROXYSCENE_MIRROR_CONTAINER_TEST') != '1':
        parser.error('set PROXYSCENE_MIRROR_CONTAINER_TEST=1 to run disposable privileged containers')
    mirror.validate_tag(args.candidate_tag)
    assert mirror.version_key(args.candidate_tag) > mirror.version_key(BASELINE_TAG)
    assert re.fullmatch('[0-9a-f]{40}', args.candidate_commit), 'invalid candidate commit'
    baseline_blob = args.baseline_bundle.read_bytes()
    assert sha(baseline_blob) == BASELINE_SHA256, 'baseline does not match reviewed official v0.11.0 bytes'
    if args.baseline_record:
        baseline_info = mirror._validate_info(mirror._decode_json(args.baseline_record.read_bytes()))
    else:
        baseline_info = mirror.release_info(BASELINE_TAG, os.environ.get('GH_TOKEN') or None)
    assert baseline_info['tag'] == BASELINE_TAG and baseline_info['commit'] == BASELINE_COMMIT
    assert baseline_info['release_id'] == 399036451
    assert baseline_info['assets']['proxyscene_bundle_linux_amd64.tar.gz']['sha256'] == BASELINE_SHA256
    assert baseline_info['assets']['proxyscene_bundle_linux_amd64.tar.gz']['size'] == len(baseline_blob)
    candidate_assets = {}
    for path in args.candidate_dir.iterdir():
        assert path.is_file() and not path.is_symlink(), path
        candidate_assets[path.name] = {'sha256': sha(path.read_bytes()), 'size': path.stat().st_size,
                                       'url': mirror.RELEASE_BASE + '/' + args.candidate_tag + '/' + path.name}
    candidate_info = {'tag': args.candidate_tag, 'version': args.candidate_tag[1:],
                      'commit': args.candidate_commit, 'release_id': 1,
                      'published_at': '2000-01-01T00:00:00Z',
                      'notes': 'ISOLATED PRE-PUBLICATION CANDIDATE FIXTURE; NOT A PUBLISHED RELEASE',
                      'assets': candidate_assets}
    mirror.verify_directory(candidate_info, args.candidate_dir)
    candidate_blob = (args.candidate_dir / 'proxyscene_bundle_linux_amd64.tar.gz').read_bytes()
    if args.evidence:
        base = args.evidence.absolute()
        base.mkdir(mode=0o700, parents=True, exist_ok=True)
        assert not list(base.iterdir()), 'evidence directory must be empty'
    else:
        base = Path(tempfile.mkdtemp(prefix='proxyscene-mirror-candidate-', dir='/tmp'))
    inputs = base / 'inputs'; inputs.mkdir(mode=0o700)
    public = inputs / 'public' / 'proxyscene'; public.mkdir(parents=True)
    shutil.copytree(args.candidate_dir, public / args.candidate_tag)
    (inputs / 'baseline.tar.gz').write_bytes(baseline_blob)
    identities = {}
    for label, info, blob in [('baseline', baseline_info, baseline_blob), ('candidate', candidate_info, candidate_blob)]:
        files = components(blob)
        identities[label] = {'tag': info['tag'], 'commit': info['commit'], 'bundle_sha256': sha(blob),
                             'manager_sha256': sha(files['proxyscene']), 'xray_sha256': sha(files['xray'])}
        (public / 'metadata').mkdir(exist_ok=True)
        (public / 'metadata' / (info['tag'] + '.json')).write_bytes(mirror.metadata_bytes(info))
        (inputs / (label + '-latest.json')).write_bytes(mirror.manifest_bytes(info))
    (inputs / 'identities.json').write_bytes(canonical(identities))
    (base / 'baseline-release.json').write_bytes(canonical(baseline_info))
    (base / 'candidate-release.json').write_bytes(canonical(candidate_info))
    readme = (ROOT / 'README.md').read_text()
    section = readme.split('### 推荐：固定镜像安装入口（无需访问 GitHub）', 1)[1]
    entry = section.split('```bash\n', 1)[1].split('```', 1)[0]
    assert entry.startswith("sudo bash -c '")
    (inputs / 'readme-entry.sh').write_text(entry)
    sources = [Path(__file__).resolve(), ROOT / 'README.md', ROOT / 'scripts/bootstrap-install.sh',
               ROOT / 'deploy/nginx/proxyscene.conf', ROOT / 'scripts/mirror_release.py']
    source_before = {str(path): sha(path.read_bytes()) for path in sources}
    (base / 'source-identities.json').write_bytes(canonical(source_before))
    for source, name in [(ROOT / 'scripts/bootstrap-install.sh', 'bootstrap-install.sh'),
                         (ROOT / 'deploy/nginx/proxyscene.conf', 'proxyscene.conf')]:
        shutil.copy2(source, inputs / name)
    print('EVIDENCE ' + str(base), flush=True)
    before = inventory(); (base / 'inventory-before.json').write_bytes(canonical(before))
    added_image_reference = False
    pulled_image_id = None
    success = False
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    try:
        inspected = subprocess.run(['docker', 'image', 'inspect', args.image], capture_output=True, text=True, timeout=30)
        if inspected.returncode != 0:
            assert args.image == DEFAULT_IMAGE, 'custom canary images must already exist'
            added_image_reference = True
            command(['docker', 'pull', DEFAULT_IMAGE], stdout=subprocess.DEVNULL)
            inspected = command(['docker', 'image', 'inspect', args.image], capture_output=True, text=True)
        image_id = json.loads(inspected.stdout)[0]['Id']
        if added_image_reference:
            pulled_image_id = image_id
        (base / 'image.json').write_text(inspected.stdout)
        for scenario in ('fresh', 'upgrade'):
            print('RUN ' + scenario, flush=True)
            run_scenario(scenario, image_id, inputs, base / scenario)
        mirror.verify_directory(candidate_info, args.candidate_dir)
        assert {str(path): sha(path.read_bytes()) for path in sources} == source_before, 'tested source changed'
        success = True
    finally:
        # Remove only the exact digest reference absent before our pull, even
        # when its image ID was already present under another tag or digest.
        # Docker retains any other preexisting references to the same image.
        if added_image_reference:
            inspection = subprocess.run(['docker', 'image', 'inspect', args.image], capture_output=True, text=True, timeout=30)
            if inspection.returncode == 0:
                current = json.loads(inspection.stdout)
                if pulled_image_id is not None:
                    assert current[0]['Id'] == pulled_image_id, 'canary image identity changed'
                command(['docker', 'image', 'rm', args.image], stdout=subprocess.DEVNULL)
            assert subprocess.run(['docker', 'image', 'inspect', args.image], capture_output=True, timeout=30).returncode != 0
        after = inventory(); (base / 'inventory-after.json').write_bytes(canonical(after))
        clean = before == after
        (base / 'result.json').write_bytes(canonical({'passed': success, 'inventory_restored': clean,
            'candidate_tag': args.candidate_tag, 'candidate_commit': args.candidate_commit,
            'baseline_tag': BASELINE_TAG, 'baseline_bundle_sha256': BASELINE_SHA256,
            'real_systemd': True, 'external_network_disconnected': True}))
        assert clean, 'Docker inventory changed during canary'
    assert success
    print('MIRROR_ONLY_CANDIDATE_CANARY_OK ' + str(base), flush=True)


if __name__ == '__main__':
    main()
