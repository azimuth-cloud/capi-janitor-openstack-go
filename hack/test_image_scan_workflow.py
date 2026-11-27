#!/usr/bin/env python3
"""Check local shared-action wiring without builds, credentials or network access."""

import os
from pathlib import Path
import re
import shlex
import subprocess
import tempfile
import unittest

import yaml


PROJECT_DIR = Path(__file__).resolve().parent.parent
SHARED_ACTION = "azimuth-cloud/github-actions/docker-multiarch-build-push"


def expression(value):
    return re.sub(r"\$\{\{(.*?)\}\}", lambda match: "${{" + re.sub(r"\s+", "", match[1]) + "}}", value)


class ImageScanWorkflowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        directory = PROJECT_DIR / ".github" / "workflows"
        cls.jobs = yaml.safe_load((directory / "build-push-artifacts.yaml").read_text())["jobs"]
        cls.caller = yaml.safe_load((directory / "test-pr.yaml").read_text())["jobs"]["publish_artifacts"]

    def setUp(self):
        self.build = self.jobs["build_push_images"]
        self.smoke = self.jobs["smoke_images"]
        self.shared = self.only([step for step in self.build["steps"] if step.get("uses", "").split("@")[0] == SHARED_ACTION])
        self.smoke_step = self.only([step for step in self.smoke["steps"] if "docker pull" in step.get("run", "")])

    def only(self, items):
        self.assertEqual(len(items), 1, items)
        return items[0]

    def assert_default_success(self, item):
        self.assertIn(item.get("continue-on-error"), (None, False, "false"))
        self.assertIn(expression(str(item.get("if", ""))), ("", "success()", "${{success()}}"))

    def version_producer(self):
        argument = expression(self.shared["with"]["build-args"])
        match = re.fullmatch(r"GO_VERSION=\$\{\{steps\.([\w-]+)\.outputs\.([\w-]+)\}\}", argument)
        self.assertIsNotNone(match, "The shared build must use the Go-version step output")
        producer = self.only([step for step in self.build["steps"] if step.get("id") == match[1]])
        self.assertIn("make --no-print-directory go-version", producer["run"])
        self.assertIn(match[2] + "=", producer["run"])
        self.assertIn("$GITHUB_OUTPUT", producer["run"])
        self.assertLess(self.build["steps"].index(producer), self.build["steps"].index(self.shared))
        return producer

    def test_one_shared_action_owns_multiarch_build_and_scanning(self):
        actions = [step.get("uses", "").split("@")[0] for job in self.jobs.values() for step in job.get("steps", [])]
        self.assertEqual(actions.count(SHARED_ACTION), 1)
        self.assertNotIn("anchore/scan-action", actions)
        self.assertNotIn("github/codeql-action/upload-sarif", actions)
        self.assertNotIn("matrix", self.build.get("strategy", {}))
        self.assertEqual(set(self.shared["with"]["platforms"].replace(" ", "").split(",")), {"linux/amd64", "linux/arm64"})
        self.assertIn(self.shared["with"]["push"], (True, "true"))
        self.assertEqual(self.shared["with"]["context"], ".")
        self.assert_default_success(self.shared)
        self.version_producer()

    def test_go_version_lookup_failure_stops_before_the_shared_build(self):
        # A failed make inside echo "$(make ...)" must not be mistaken for success.
        producer = self.version_producer()
        with tempfile.TemporaryDirectory() as temporary_directory:
            directory = Path(temporary_directory)
            make = directory / "make"
            make.write_text("#!/bin/sh\nexit 42\n")
            make.chmod(0o755)
            environment = dict(os.environ, PATH=str(directory) + os.pathsep + os.environ.get("PATH", ""), GITHUB_OUTPUT=str(directory / "outputs"))
            result = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", producer["run"]], cwd=directory, env=environment, capture_output=True, check=False)
            self.assertNotEqual(result.returncode, 0)

    def test_native_smokes_pull_the_same_short_sha_image_without_rebuilding(self):
        image = expression(self.smoke_step["env"]["IMAGE"])
        reference = re.fullmatch(r"\$\{\{needs\.build_push_images\.outputs\.([\w-]+)\}\}", image)
        self.assertIsNotNone(reference)
        published_image = expression(self.build["outputs"][reference[1]])
        self.assertRegex(published_image, r"^ghcr\.io/\$\{\{github\.repository\}\}:\$\{\{steps\.[\w-]+\.outputs\.short-sha\}\}$")
        runners = {entry["arch"]: entry["runs-on"] for entry in self.smoke["strategy"]["matrix"]["include"]}
        self.assertEqual(set(runners), {"amd64", "arm64"})
        self.assertIn("arm", runners["arm64"])
        self.assertNotIn("arm", runners["amd64"])
        self.assertEqual(expression(self.smoke["runs-on"]), "${{matrix.runs-on}}")
        scripts = "\n".join(step.get("run", "") for step in self.smoke["steps"])
        self.assertNotRegex(scripts, r"\b(?:make\s+docker-build\w*|docker\s+(?:build|buildx\s+build))\b")
        self.assert_default_success(self.smoke_step)

    def test_pulled_image_runs_both_entrypoints_under_runtime_restrictions(self):
        commands = [shlex.split(line) for line in self.smoke_step["run"].splitlines() if line.strip().startswith("docker ")]
        self.assertEqual(commands[0][:2], ["docker", "pull"])
        self.assertEqual(commands[0][-3:], ["--platform", "$PLATFORM", "$IMAGE"])
        self.assertEqual(expression(self.smoke_step["env"]["PLATFORM"]), "linux/${{matrix.arch}}")
        runs = [command for command in commands if command[:2] == ["docker", "run"]]
        self.assertEqual(len(runs), 2)
        self.assertEqual(sum("--entrypoint" in command for command in runs), 1)
        for command in runs:
            with self.subTest(command=command):
                for option in ("--pull=never", "--rm", "--read-only"):
                    self.assertIn(option, command)
                self.assertEqual(command[command.index("--platform") + 1], "$PLATFORM")
                self.assertEqual(command[command.index("--user") + 1], "65532:65532")
                self.assertEqual(command[-2:], ["$IMAGE", "--help"])
                if "--entrypoint" in command:
                    self.assertEqual(command[command.index("--entrypoint") + 1], "/bin/manager")

    def test_existing_image_tags_and_architecture_sboms_are_preserved(self):
        metadata = self.only([step for step in self.build["steps"] if step.get("uses", "").startswith("docker/metadata-action@")])
        self.assertEqual(expression(metadata["with"]["images"]), "ghcr.io/${{github.repository}}")
        self.assertEqual(expression(self.shared["with"]["tags"]), "${{steps." + metadata["id"] + ".outputs.tags}}")
        tags = expression(metadata["with"]["tags"])
        for tag in ("type=ref,event=branch", "type=ref,event=tag"):
            self.assertIn(tag, tags)
        self.assertRegex(tags, r"type=raw,value=\$\{\{steps\.[\w-]+\.outputs\.short-sha\}\}")
        sbom = self.only([step for step in self.smoke["steps"] if "make sbom" in step.get("run", "")])
        self.assertEqual(expression(sbom["env"]["IMAGE"]), expression(self.smoke_step["env"]["IMAGE"]))
        self.assertIn("PLATFORM=linux/${{matrix.arch}}", expression(sbom["run"]))
        artifact = self.only([step for step in self.smoke["steps"] if step.get("uses", "").startswith("actions/upload-artifact@")])
        self.assertIn("${{matrix.arch}}", expression(artifact["with"]["name"]))
        self.assertTrue(artifact["with"]["path"].endswith(".cdx.json"))

    def test_caller_grants_shared_signing_and_sarif_permissions(self):
        for permission in ("id-token", "security-events", "packages"):
            with self.subTest(permission=permission):
                self.assertEqual(self.build["permissions"].get(permission), "write")
                self.assertEqual(self.caller["permissions"].get(permission), "write")

    def test_failed_shared_action_or_native_smoke_blocks_chart_and_release(self):
        for consumer, producers in (
            ("smoke_images", {"build_push_images"}),
            ("build_push_chart", {"build_push_images", "smoke_images"}),
            ("publish_github_release", {"build_push_images", "smoke_images", "build_push_chart"}),
        ):
            with self.subTest(consumer=consumer):
                job = self.jobs[consumer]
                needs = job.get("needs", [])
                self.assertTrue(producers.issubset({needs} if isinstance(needs, str) else set(needs)))
                self.assertIn(job.get("continue-on-error"), (None, False, "false"))
                condition = expression(str(job.get("if", "")))
                if condition not in ("", "success()", "${{success()}}"):
                    self.assertNotRegex(condition, r"(?:always|failure|cancelled|success)\s*\(")
        self.assert_default_success(self.build)


if __name__ == "__main__":
    unittest.main(verbosity=2)
