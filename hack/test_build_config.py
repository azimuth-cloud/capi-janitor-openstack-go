#!/usr/bin/env python3
"""Check build inputs using real Make recipes and a recording container tool.

These tests do not build or publish images. They exercise version and platform
propagation without requiring Docker, Podman, or network access.
"""

import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest


PROJECT_DIR = Path(__file__).resolve().parent.parent
GO_DIRECTIVE = re.compile(r"^go[ \t]+(\d+\.\d+(?:\.\d+)?)[ \t]*$", re.MULTILINE)


class BuildConfigurationTests(unittest.TestCase):
    def setUp(self):
        self.temporary_directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary_directory.cleanup)
        self.project_dir = Path(self.temporary_directory.name)
        for name in ("Makefile", "Dockerfile", "go.mod", "go.sum"):
            shutil.copyfile(PROJECT_DIR / name, self.project_dir / name)

        self.capture_path = self.project_dir / "container-commands.jsonl"
        self.container_tool = self.project_dir / "record-container-command"
        self.container_tool.write_text(
            f"#!{sys.executable}\n"
            "import json, os, sys\n"
            "with open(os.environ['CONTAINER_COMMAND_CAPTURE'], 'a') as capture:\n"
            "    capture.write(json.dumps(sys.argv[1:]) + '\\n')\n"
        )
        self.container_tool.chmod(0o755)

    def run_make(self, target, **variables):
        self.capture_path.unlink(missing_ok=True)
        environment = os.environ.copy()
        # Avoid inherited recursive-Make options and automatic Go downloads.
        for name in ("MAKEFLAGS", "MFLAGS", "MAKELEVEL", "GO_VERSION"):
            environment.pop(name, None)
        environment.update(
            GOTOOLCHAIN="local",
            CONTAINER_COMMAND_CAPTURE=str(self.capture_path),
        )
        arguments = {
            "CONTAINER_TOOL": str(self.container_tool),
            "IMG": "example.invalid/janitor:build-config-test",
            "ARCH": "arm64",
            "PLATFORM": "linux/arm64",
            "PLATFORMS": "linux/amd64,linux/arm64",
        }
        arguments.update(variables)
        result = subprocess.run(
            ["make", "--no-print-directory", target]
            + [f"{name}={value}" for name, value in arguments.items()],
            cwd=self.project_dir,
            env=environment,
            capture_output=True,
            text=True,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(self.capture_path.is_file(), "Make did not invoke CONTAINER_TOOL")
        return [json.loads(line) for line in self.capture_path.read_text().splitlines()]

    def build_arguments(self, commands):
        builds = [command for command in commands if "build" in command]
        self.assertEqual(len(builds), 1, commands)
        command = builds[0]
        return [
            command[index + 1]
            for index, argument in enumerate(command[:-1])
            if argument == "--build-arg"
        ]

    def test_go_mod_update_reaches_native_and_multiarch_builds(self):
        module_path = self.project_dir / "go.mod"
        module = module_path.read_text()
        match = GO_DIRECTIVE.search(module)
        self.assertIsNotNone(match, "go.mod must declare the required Go version")
        original_version = match.group(1)
        parts = original_version.split(".")
        updated_version = ".".join(parts[:2] + [str(int(parts[2]) + 1 if len(parts) == 3 else 1)])

        for target in ("docker-build", "docker-buildx"):
            with self.subTest(target=target, version=original_version):
                arguments = self.build_arguments(self.run_make(target))
                self.assertIn("GO_VERSION=" + original_version, arguments)

        module_path.write_text(
            module[: match.start(1)] + updated_version + module[match.end(1) :]
        )
        for target in ("docker-build", "docker-buildx"):
            with self.subTest(target=target, version=updated_version):
                arguments = self.build_arguments(self.run_make(target))
                self.assertIn("GO_VERSION=" + updated_version, arguments)
                self.assertNotIn("GO_VERSION=" + original_version, arguments)

    def test_native_platform_reaches_the_go_cross_compiler(self):
        for architecture in ("amd64", "arm64"):
            with self.subTest(architecture=architecture):
                commands = self.run_make("docker-build", PLATFORM="linux/" + architecture)
                self.assertIn("TARGETARCH=" + architecture, self.build_arguments(commands))
                command = next(command for command in commands if "build" in command)
                self.assertEqual(command[command.index("--platform") + 1], "linux/" + architecture)

    def test_export_uses_the_selected_container_tool_and_image(self):
        archive_path = self.project_dir / "manager.tar"
        commands = self.run_make("docker-save", IMAGE_ARCHIVE=archive_path)
        self.assertEqual(len(commands), 1, commands)
        command = commands[0]
        self.assertEqual(command[0], "save")
        self.assertEqual(command[command.index("--output") + 1], str(archive_path))
        self.assertIn("example.invalid/janitor:build-config-test", command)

    def docker_instructions(self):
        contents = (self.project_dir / "Dockerfile").read_text()
        # Join continued instructions before checking their effective inputs.
        contents = re.sub(r"\\\s*\n\s*", " ", contents)
        return [
            line.strip()
            for line in contents.splitlines()
            if line.strip() and not line.lstrip().startswith("#")
        ]

    def test_builder_requires_the_module_go_version_without_auto_upgrade(self):
        instructions = self.docker_instructions()
        self.assertIn("ARG GO_VERSION", instructions)
        builder = next(line for line in instructions if line.upper().startswith("FROM "))
        self.assertRegex(builder, r"\bgolang:\$\{GO_VERSION\}(?:[-@][^\s]+)?(?:\s|$)")
        self.assertTrue(
            any(
                line.upper().startswith("ENV ") and "GOTOOLCHAIN=local" in line.split()
                for line in instructions
            ),
            "The builder must fail on an incompatible Go version instead of downloading another toolchain",
        )

    def test_module_files_invalidate_the_dependency_download_layer(self):
        instructions = self.docker_instructions()
        copies = [
            index
            for index, line in enumerate(instructions)
            if line.upper().startswith("COPY ") and "go.mod" in line and "go.sum" in line
        ]
        self.assertTrue(copies, "Copy go.mod and go.sum into the dependency download layer")
        downloads = [
            index
            for index, line in enumerate(instructions)
            if line.upper().startswith("RUN ") and "go mod download" in line
        ]
        self.assertTrue(downloads, "The builder must download the declared Go modules")
        self.assertLess(copies[0], downloads[0])


if __name__ == "__main__":
    unittest.main(verbosity=2)
