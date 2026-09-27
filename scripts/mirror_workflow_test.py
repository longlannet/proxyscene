"""Execute the actual workflow shell blocks with isolated command fixtures."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


WORKFLOW = Path(__file__).resolve().parents[1] / ".github/workflows/mirror-publish.yml"
RELEASE_WORKFLOW = WORKFLOW.with_name("release.yml")
DISPATCH_WORKFLOW = WORKFLOW.with_name("mirror-release.yml")


def mapping_block(name, lines, indent):
    """Read a block mapping from the repository's actionlint-validated YAML."""
    start = lines.index(" " * indent + name + ":")
    body = []
    for line in lines[start + 1:]:
        if line and not line.startswith(" " * (indent + 2)):
            break
        body.append(line)
    return body


def direct_entries(lines, indent):
    entries = {}
    for line in lines:
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if len(line) - len(line.lstrip()) == indent:
            name, separator, value = line.strip().partition(":")
            if not separator or name in entries:
                raise AssertionError("invalid or duplicate mapping entry: " + line)
            entries[name] = value.strip()
    return entries


def block(name, workflow=WORKFLOW):
    lines = workflow.read_text().splitlines()
    start = lines.index("      - name: " + name)
    for start in range(start + 1, len(lines)):
        if lines[start] == "        run: |":
            break
    else:
        raise AssertionError("missing run block: " + name)
    body = []
    for line in lines[start + 1:]:
        if line and not line.startswith("          "):
            break
        body.append(line[10:] if line else "")
    return "\n".join(body) + "\n"


class WorkflowTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="proxyscene-workflow-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.calls = self.root / "calls"
        self.environment = {**os.environ, "RUNNER_TEMP": str(self.root),
                            "TAG": "v0.9.0", "MIRROR_HOST": "mirror.example",
                            "MIRROR_PORT": "22", "MIRROR_USER": "psmirror",
                            "MIRROR_SSH_KEY": "fixture-private-key", "MIRROR_KNOWN_HOSTS": "fixture-host-pin",
                            "GITHUB_STEP_SUMMARY": str(self.root / "summary"),
                            "GITHUB_REPOSITORY": "longlannet/proxyscene", "GITHUB_EVENT_NAME": "workflow_dispatch",
                            "GITHUB_REF": "refs/heads/main", "GITHUB_SHA": "a" * 40,
                            "TRUSTED_WORKFLOW_SHA": "a" * 40, "MIRROR_CONFIGURED": "true",
                            "CALLS": str(self.calls), "LATEST_TAG": "v0.9.0", "FAIL_STAGE": ""}

    def stub(self, name, body):
        path = self.bin / name
        path.write_text("#!/bin/bash\nset -eu\n" + body)
        path.chmod(0o755)

    def execute(self, name, workflow=WORKFLOW):
        return subprocess.run(["bash", "-c", block(name, workflow)], env=self.environment,
                              text=True, capture_output=True, timeout=20)

    def install_stubs(self):
        self.environment["PATH"] = str(self.bin) + ":/usr/bin:/bin"
        self.stub("ssh-keygen", "exit 0\n")
        self.stub("gh", 'printf "%s\\n" "$LATEST_TAG"\n')
        self.stub("ssh", r'''
command="${!#}"
printf '%s\n' "$command" >> "$CALLS"
[[ -z "${MIRROR_SSH_KEY+x}" && -z "${MIRROR_KNOWN_HOSTS+x}" ]]
for candidate in "$RUNNER_TEMP"/proxyscene-mirror-ssh.*/key; do
    [[ "$(stat -c '%a' "$candidate")" == 600 ]]
done
if [[ "$command" == *' sync '* && "$FAIL_STAGE" == sync ]]; then exit 41; fi
if [[ "$command" == *' promote '* && "$FAIL_STAGE" == promote ]]; then exit 42; fi
''')
        self.stub("python3", r'''
case " $* " in
    *' --stable '*) stage=stable ;;
    *) stage=public ;;
esac
printf 'verify %s\n' "$stage" >> "$CALLS"
[[ "$FAIL_STAGE" != "$stage" ]]
''')

    def test_only_trusted_entry_configuration_is_accepted(self):
        result = self.execute("Validate trusted source and deployment configuration")
        self.assertEqual(result.returncode, 0, result.stderr)
        cases = {"GITHUB_REF": "refs/tags/v0.9.0", "GITHUB_EVENT_NAME": "pull_request",
                 "GITHUB_REPOSITORY": "attacker/proxyscene", "GITHUB_SHA": "b" * 40,
                 "MIRROR_CONFIGURED": "false", "TAG": "v0.9.0;touch injected",
                 "MIRROR_HOST": "-oProxyCommand=bad", "MIRROR_PORT": "022", "MIRROR_USER": "root@other"}
        for key, value in cases.items():
            with self.subTest(variable=key):
                original = self.environment[key]
                self.environment[key] = value
                result = self.execute("Validate trusted source and deployment configuration")
                self.environment[key] = original
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((self.root / "injected").exists())

    def test_release_requires_configured_mirror(self):
        step = "Require configured mirror before release"
        result = self.execute(step, RELEASE_WORKFLOW)
        self.assertEqual(result.returncode, 0, result.stderr)
        for value in (None, "", "false", "TRUE", "true ", "1"):
            with self.subTest(configured=value):
                if value is None:
                    self.environment.pop("MIRROR_CONFIGURED", None)
                else:
                    self.environment["MIRROR_CONFIGURED"] = value
                result = self.execute(step, RELEASE_WORKFLOW)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("PROXYSCENE_RELEASE_MIRROR_CONFIGURED=true", result.stderr)

    def test_release_and_retry_use_the_same_worker_and_only_named_mirror_secrets(self):
        expected_secrets = {
            name: "${{ secrets." + name + " }}"
            for name in ("MIRROR_SSH_KEY", "MIRROR_KNOWN_HOSTS")
        }
        for workflow in (RELEASE_WORKFLOW, DISPATCH_WORKFLOW):
            with self.subTest(workflow=workflow.name):
                jobs = mapping_block("jobs", workflow.read_text().splitlines(), 0)
                mirror = mapping_block("mirror", jobs, 2)
                entries = direct_entries(mirror, 4)
                self.assertEqual(entries["uses"], "./.github/workflows/" + WORKFLOW.name)
                self.assertNotIn("if", entries)
                self.assertEqual(direct_entries(mapping_block("secrets", mirror, 4), 6), expected_secrets)
                self.assertEqual(direct_entries(mapping_block("permissions", mirror, 4), 6), {"contents": "read"})
                if workflow == RELEASE_WORKFLOW:
                    self.assertEqual(entries["needs"], "verify")

        lines = WORKFLOW.read_text().splitlines()
        workflow_call = mapping_block("workflow_call", mapping_block("on", lines, 0), 2)
        secret_declarations = mapping_block("secrets", workflow_call, 4)
        self.assertEqual(set(direct_entries(secret_declarations, 6)), set(expected_secrets))
        for name in expected_secrets:
            self.assertEqual(direct_entries(mapping_block(name, secret_declarations, 6), 8), {"required": "false"})
        worker = mapping_block("mirror", mapping_block("jobs", lines, 0), 2)
        self.assertEqual(direct_entries(worker, 4)["environment"], "release-mirror")

    def test_sync_and_public_verification_precede_promotion_and_cleanup(self):
        self.install_stubs()
        result = self.execute("Synchronize, verify public bytes, then promote Latest")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls.read_text().splitlines(),
                         ["proxyscene-mirror sync v0.9.0", "verify public",
                          "proxyscene-mirror promote v0.9.0", "verify stable"])
        self.assertEqual(list(self.root.glob("proxyscene-mirror-ssh.*")), [])

    def test_failure_at_each_stage_stops_the_remaining_actions(self):
        self.install_stubs()
        full = ["proxyscene-mirror sync v0.9.0", "verify public", "proxyscene-mirror promote v0.9.0", "verify stable"]
        for index, stage in enumerate(("sync", "public", "promote", "stable")):
            with self.subTest(stage=stage):
                self.calls.unlink(missing_ok=True)
                self.environment["FAIL_STAGE"] = stage
                result = self.execute("Synchronize, verify public bytes, then promote Latest")
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.calls.read_text().splitlines(), full[:index + 1])
                self.assertEqual(list(self.root.glob("proxyscene-mirror-ssh.*")), [])

    def test_old_release_is_archived_without_changing_latest(self):
        self.install_stubs()
        self.environment["LATEST_TAG"] = "v0.10.0"
        result = self.execute("Synchronize, verify public bytes, then promote Latest")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls.read_text().splitlines(), ["proxyscene-mirror sync v0.9.0", "verify public"])
        self.assertEqual(list(self.root.glob("proxyscene-mirror-ssh.*")), [])

    def test_missing_credentials_fail_before_contacting_mirror(self):
        self.install_stubs()
        for name in ("MIRROR_SSH_KEY", "MIRROR_KNOWN_HOSTS"):
            original = self.environment[name]
            for value in (None, ""):
                with self.subTest(credential=name, value=value):
                    if value is None:
                        self.environment.pop(name, None)
                    else:
                        self.environment[name] = value
                    result = self.execute("Synchronize, verify public bytes, then promote Latest")
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("Mirror credentials are unavailable", result.stderr)
                    self.assertIn("release-mirror Environment", result.stderr)
                    self.assertIn("named secret mappings", result.stderr)
                    self.assertNotIn("fixture-private-key", result.stderr)
                    self.assertFalse(self.calls.exists())
                    self.assertEqual(list(self.root.glob("proxyscene-mirror-ssh.*")), [])
            self.environment[name] = original


if __name__ == "__main__":
    unittest.main()
