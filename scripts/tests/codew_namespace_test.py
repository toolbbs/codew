"""CLI namespace rejection with synthetic files and fail-closed system substitutes."""
import hashlib
import json
import os
import shlex
import shutil
import stat
import subprocess
import tarfile
import unittest

import cli_reliability_test as fixtures


class CodewNamespace(unittest.TestCase):
    def setUp(self):
        self.fixture = fixtures.CliReliability("test_health_requires_exact_service_version_and_json")
        self.addCleanup(self.fixture.doCleanups)
        self.fixture.setUp()
        self.queries = self.fixture.root / "namespace-queries.log"
        self.queries.write_text("")
        for name in ("find", "sort", "du", "awk"):
            tool = shutil.which(name)
            self.assertIsNotNone(tool, "Existing tool required: " + name)
            (self.fixture.bin / name).symlink_to(tool)

    def snapshot(self):
        result = {}
        excluded = {self.fixture.trace, self.queries}
        for path in [self.fixture.root, *self.fixture.root.rglob("*")]:
            if path in excluded:
                continue
            info = path.lstat()
            payload = os.readlink(path) if stat.S_ISLNK(info.st_mode) else (
                hashlib.sha256(path.read_bytes()).hexdigest() if stat.S_ISREG(info.st_mode) else None)
            result[str(path.relative_to(self.fixture.root))] = (info.st_mode, info.st_mtime_ns, payload)
        return result

    def rejected(self, action, archive=None, settings=None, message=None):
        self.fixture.trace.write_text("")
        self.queries.write_text("")
        before = self.snapshot()
        setup = r'''
systemctl() {
  if [[ "$1" == show ]]; then
    printf '%s\n' "$*" >> "$LAB/namespace-queries.log"
    case "$2:$4" in
      zxy-panel-api:LoadState|zxy-agent:LoadState|zxy-panel:LoadState)
        if [[ "$2" == "$NAMESPACE_UNIT" ]]; then
          [[ "$NAMESPACE_ERROR" != 1 ]] || return 17
          printf '%s\n' "$NAMESPACE_STATE"
        else printf 'not-found\n'; fi
        return;;
      xray:ExecStart) printf '%s\n' "$NAMESPACE_XRAY"; return;;
    esac
  fi
  printf 'FORBIDDEN systemctl %s\n' "$*" >> "$TRACE"; return 97
}
curl() { printf 'FORBIDDEN curl\n' >> "$TRACE"; return 97; }
nginx() { printf 'FORBIDDEN nginx\n' >> "$TRACE"; return 97; }
docker() { printf 'FORBIDDEN docker\n' >> "$TRACE"; return 97; }
apt-get() { printf 'FORBIDDEN apt-get\n' >> "$TRACE"; return 97; }
rm() { printf 'FORBIDDEN rm\n' >> "$TRACE"; return 97; }
mkdir() { printf 'FORBIDDEN mkdir\n' >> "$TRACE"; return 97; }
mktemp() { printf 'FORBIDDEN mktemp\n' >> "$TRACE"; return 97; }
cp() { printf 'FORBIDDEN cp\n' >> "$TRACE"; return 97; }
mv() { printf 'FORBIDDEN mv\n' >> "$TRACE"; return 97; }
chmod() { printf 'FORBIDDEN chmod\n' >> "$TRACE"; return 97; }
tee() { printf 'FORBIDDEN tee\n' >> "$TRACE"; return 97; }
export -f systemctl curl nginx docker apt-get rm mkdir mktemp cp mv chmod tee
'''
        argv = ["bash", str(self.fixture.cli), action]
        if archive is not None:
            argv.append(str(archive))
        env = dict(self.fixture.env, NAMESPACE_UNIT="zxy-panel-api", NAMESPACE_ERROR="0",
                   NAMESPACE_STATE="not-found", NAMESPACE_XRAY="")
        env.update(settings or {})
        result = subprocess.run(["bash", "-euc", setup + "\nexec " + shlex.join(argv)],
                                env=env, capture_output=True, text=True, timeout=15)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertNotIn("command not found", result.stderr)
        if message is not None:
            self.assertIn(message, result.stderr)
        self.assertNotIn("Backup created:", result.stdout)
        self.assertNotIn("restore completed", result.stdout)
        self.assertNotIn("uninstalled.", result.stdout)
        self.assertEqual(self.fixture.trace.read_text(), "", result.stderr)
        self.assertEqual(self.snapshot(), before, "Rejected CLI input changed files, directories or metadata")
        return result

    def rewrite_metadata(self, archive, replacements):
        def update(items):
            output = []
            for member, value in items:
                if member.name == "manifest.json":
                    data = json.loads(value)
                    data.update(replacements)
                    value = json.dumps(data).encode()
                output.append((member, value))
            return output
        return self.fixture.rewritten_archive(archive, update)

    def test_clean_cli_backup_creates_only_codew_archive(self):
        before = self.fixture.snapshot()
        result = self.fixture.invoke("backup")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.fixture.snapshot(), before)
        archives = list((self.fixture.app / "backups").glob("*.tar.gz"))
        self.assertEqual(len(archives), 1)
        self.assertTrue(archives[0].name.startswith("codew-backup-"))
        with tarfile.open(archives[0]) as source:
            self.assertEqual(json.load(source.extractfile("manifest.json"))["format"], "codew-config-data-v1")
        self.assertFalse(list((self.fixture.app / "backups").glob(".codew-backup-*")))

    def test_legacy_path_types_block_every_mutating_entrypoint(self):
        legacy = self.fixture.root / "legacy" / "0"
        legacy.parent.mkdir()
        for kind in ("directory", "file", "symlink"):
            if kind == "directory":
                legacy.mkdir()
            elif kind == "file":
                legacy.write_text("synthetic legacy data")
            else:
                legacy.symlink_to(self.fixture.root / "absent-legacy-target")
            for action in ("backup", "restore", "update", "uninstall"):
                with self.subTest(kind=kind, action=action):
                    self.rejected(action, message="legacy installation detected")
                    self.assertEqual(self.queries.read_text(), "")
            if kind == "directory":
                legacy.rmdir()
            else:
                legacy.unlink()

    def test_legacy_service_states_block_every_mutating_entrypoint(self):
        for unit in ("zxy-panel-api", "zxy-agent", "zxy-panel"):
            for state in ("loaded", "masked", ""):
                for action in ("backup", "restore", "update", "uninstall"):
                    with self.subTest(unit=unit, state=state, action=action):
                        self.rejected(action, settings={"NAMESPACE_UNIT": unit, "NAMESPACE_STATE": state},
                                      message="legacy service detected or state is unknown")

    def test_inspection_failure_is_not_treated_as_absence(self):
        for action in ("backup", "restore", "update", "uninstall"):
            with self.subTest(action=action):
                self.rejected(action, settings={"NAMESPACE_ERROR": "1"}, message="cannot verify legacy service state")

    def test_legacy_xray_ownership_blocks_every_mutating_entrypoint(self):
        for action in ("backup", "restore", "update", "uninstall"):
            with self.subTest(action=action):
                self.rejected(action, settings={"NAMESPACE_XRAY": "/usr/local/bin/xray run -config /etc/zxy-panel/xray/config.json"},
                              message="Xray belongs to a legacy installation")

    def test_legacy_format_rejected_before_any_recovery_directory(self):
        archive = self.fixture.backup()
        for replacements in (
            {"format": "zxy-panel-config-data-v1"},
            {"format": "zxy-panel-config-data-v1", "install_dir": "/opt/zxy-panel", "config_dir": "/etc/zxy-panel"},
        ):
            with self.subTest(metadata=replacements):
                bad = self.rewrite_metadata(archive, replacements)
                self.rejected("restore", bad, message="no recovery directory created and no writer stopped")

    def test_foreign_metadata_paths_rejected_before_any_recovery_directory(self):
        archive = self.fixture.backup()
        for key, path in (("install_dir", "/opt/zxy-panel"), ("config_dir", "/etc/zxy-panel"),
                          ("database_path", "/opt/zxy-panel/data/zxy-panel.json")):
            with self.subTest(key=key):
                bad = self.rewrite_metadata(archive, {key: path})
                self.rejected("restore", bad, message="no recovery directory created and no writer stopped")

    def test_invalid_new_archive_rejected_before_staging(self):
        archive = self.fixture.backup()
        for change in (
            lambda items: [(m, b'{"bad_hash":true}' if m.name == "files/database" else value) for m, value in items],
            lambda items: items + [items[0]],
            lambda items: items + [(tarfile.TarInfo("../outside"), b"synthetic")],
        ):
            with self.subTest(case=repr(change)):
                bad = self.fixture.rewritten_archive(archive, change)
                self.rejected("restore", bad, message="no recovery directory created and no writer stopped")

    def test_discovery_does_not_select_or_delete_legacy_filenames(self):
        current = self.fixture.backup()
        legacy = current.parent / "zxy-panel-backup-legacy.tar.gz"
        shutil.copy2(current, legacy)
        os.utime(current, ns=(1000000000, 1000000000))
        os.utime(legacy, ns=(2000000000, 2000000000))
        before = self.snapshot()
        result = self.fixture.shell("latest_backup_file; print_backup_list")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.splitlines()[0], str(current))
        self.assertNotIn(legacy.name, result.stdout)
        self.assertIn(current.name, result.stdout)
        self.assertEqual(self.snapshot(), before)


if __name__ == "__main__":
    unittest.main(verbosity=2)
