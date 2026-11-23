"""Real Go-generated runner/state logic, restricted to synthetic directories."""
import contextlib
import base64
import datetime
import hashlib
import importlib.util
import io
import json
import multiprocessing
import os
from pathlib import Path
import shutil
import signal
import socket
import stat
import subprocess
import sys
import tarfile
import tempfile
import types
import unittest
import zipfile
from unittest import mock


def generated_fixture():
    path = os.environ.get("CODEW_UPGRADE_TEST_EVIDENCE")
    if not path:
        raise RuntimeError("Generate the Go state fixture and set CODEW_UPGRADE_TEST_EVIDENCE first")
    return json.loads((Path(path) / "state-fixture.json").read_text(encoding="utf-8"))


def state_module(program):
    module = types.ModuleType("codew_upgrade_state")
    exec(compile(program, "<Go-generated-task-state>", "exec"), module.__dict__)
    return module


def reserve_worker(program, root, work, task, barrier, queue):
    module = state_module(program)
    barrier.wait(timeout=10)
    try:
        module.TaskState(root, work).apply("reserve", task["id"], os.getpid(), task)
        queue.put("reserved")
    except module.TaskError:
        queue.put("blocked")


class UpgradeStateTransactions(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.generated = generated_fixture()

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="codew-upgrade-state-")
        self.base = Path(self.temp.name)
        self.root, self.work = self.base / "app", self.base / "work"
        self.root.mkdir()
        (self.root / "data").mkdir()
        (self.root / "logs").mkdir()
        for name in ("data/codew.json", ".env", "logs/previous.log", "bbr.conf"):
            (self.root / name).write_text("synthetic protected content " + name)
        self.protected = self.snapshot()
        self.module = state_module(self.generated["state_program"])
        self.store = self.module.TaskState(str(self.root), str(self.work))
        self.task = self.task_value("1111111111111111")

    def tearDown(self):
        result = self._outcome.result
        failed = any(test is self for test, _ in result.failures + result.errors)
        changed = self.snapshot() != self.protected
        if changed:
            failed = True
        if failed:
            evidence = Path(os.environ["CODEW_UPGRADE_TEST_EVIDENCE"])
            shutil.copytree(self.base, evidence / (self._testMethodName + "-failed-fixture"), symlinks=True)
        self.temp.cleanup()
        if changed:
            self.fail("Task transaction changed business data, configuration, old logs or BBR")

    def snapshot(self):
        return {name: (hashlib.sha256((self.root / name).read_bytes()).hexdigest(),
                       (self.root / name).stat().st_mtime_ns, (self.root / name).stat().st_mode)
                for name in ("data/codew.json", ".env", "logs/previous.log", "bbr.conf")}

    def task_value(self, suffix):
        manifest = dict(self.generated["manifest"])
        task_id = "upgrade_20261117_120000_" + suffix
        now = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        return dict(id=task_id, status="pending", stage="created", message="Synthetic task.",
                    current_version="0.7.8-stable-engineering", target_version=manifest["latest"],
                    package=manifest["package"], download_url=manifest["download_url"], sha256=manifest["sha256"],
                    manifest=manifest, log_file=str(self.root / "logs/upgrades" / task_id / "runner.log"),
                    work_dir=str(self.work / task_id), started_at=now, updated_at=now)

    def reserve(self):
        return self.store.apply("reserve", self.task["id"], os.getpid(), self.task)

    def operation(self, name, **payload):
        return self.store.apply(name, self.task["id"], os.getpid(), payload)

    def current(self):
        return self.module.load(self.store.latest)

    def latest_snapshot(self):
        info = self.store.latest.stat()
        return self.store.latest.read_bytes(), info.st_mode, info.st_mtime_ns

    def test_read_does_not_create_coordination_or_state(self):
        self.assertIsNone(self.current())
        self.assertFalse((self.root / "data/.upgrade-state.lock").exists())
        self.assertFalse(self.store.reservation.exists())

    def test_persistent_reservation_blocks_another_manager(self):
        self.reserve()
        before = self.latest_snapshot()
        restarted = self.module.TaskState(str(self.root), str(self.work))
        other = self.task_value("2222222222222222")
        with self.assertRaises(self.module.TaskError):
            restarted.apply("reserve", other["id"], os.getpid(), other)
        self.assertEqual(self.latest_snapshot(), before)
        self.assertEqual(self.module.load(self.store.reservation / "owner.json")["id"], self.task["id"])

    def test_atomic_write_and_rename_failures_keep_previous_bytes_and_time(self):
        self.reserve()
        before = self.latest_snapshot()
        changed = dict(self.current(), stage="candidate")
        for function in ("fsync", "replace"):
            with self.subTest(function=function):
                with mock.patch.object(self.module.os, function, side_effect=OSError("synthetic publication failure")):
                    with self.assertRaises(OSError):
                        self.module.publish(self.store.latest, changed)
                self.assertEqual(self.latest_snapshot(), before)
                self.assertTrue(self.store.reservation.exists())

    def test_post_publication_failure_is_unknown_not_unchanged(self):
        self.reserve()
        before = self.latest_snapshot()
        changed = dict(self.current(), stage="committed-candidate")
        original = os.fsync
        calls = []
        def fsync(fd):
            calls.append(fd)
            if len(calls) == 2:
                raise OSError("synthetic directory sync failure")
            return original(fd)
        with mock.patch.object(self.module.os, "fsync", side_effect=fsync):
            with self.assertRaises(self.module.CommitUnknown):
                self.module.publish(self.store.latest, changed)
        self.assertEqual(self.current()["stage"], "committed-candidate")
        self.assertNotEqual(self.latest_snapshot()[0], before[0])
        self.assertTrue(self.store.reservation.exists())

    def test_foreign_id_and_duplicate_claim_cannot_overwrite_state(self):
        self.reserve()
        self.operation("claim")
        before = self.latest_snapshot()
        with self.assertRaises(self.module.TaskError):
            self.store.apply("progress", "upgrade_20261117_120000_2222222222222222", os.getpid(), dict(status="downloading"))
        with self.assertRaises(self.module.TaskError):
            self.operation("claim")
        self.assertEqual(self.latest_snapshot(), before)

    def test_success_requires_verification_stage_and_terminal_is_frozen(self):
        self.reserve()
        self.operation("claim")
        before = self.latest_snapshot()
        with self.assertRaises(self.module.TaskError):
            self.operation("finish", status="success", stage="success", exit_code=0)
        self.assertEqual(self.latest_snapshot(), before)
        self.operation("progress", status="restarting", stage="verifying", exit_code=0)
        result = self.operation("finish", status="success", stage="success", exit_code=0, message="Synthetic verified success.")
        self.assertEqual(result["status"], "success")
        self.assertFalse(self.store.reservation.exists())
        self.assertTrue((self.store.history / self.task["id"] / "owner.json").is_file())
        finished = self.latest_snapshot()
        with self.assertRaises(self.module.TaskError):
            self.operation("progress", status="running", stage="queued", exit_code=0)
        self.assertEqual(self.latest_snapshot(), finished)

    def test_live_runner_and_creator_cannot_be_cleared(self):
        self.task["updated_at"] = "2000-01-01T00:00:00Z"
        self.reserve()
        before = self.latest_snapshot()
        with self.assertRaises(self.module.TaskError):
            self.operation("clear", unit_stopped=True, status="failed", stage="stale_cleaned", exit_code=98)
        self.assertEqual(self.latest_snapshot(), before)
        self.operation("claim")
        before = self.latest_snapshot()
        with self.assertRaises(self.module.TaskError):
            self.operation("clear", unit_stopped=True, status="failed", stage="stale_cleaned", exit_code=98)
        self.assertEqual(self.latest_snapshot(), before)

    def test_unknown_launch_keeps_reservation_and_evidence(self):
        self.task["updated_at"] = "2000-01-01T00:00:00Z"
        self.reserve()
        before = self.latest_snapshot()
        self.operation("launch_unknown")
        with self.assertRaises(self.module.TaskError):
            self.operation("clear", unit_stopped=True, status="failed", stage="stale_cleaned", exit_code=98)
        self.assertEqual(self.latest_snapshot(), before)
        self.assertTrue(self.store.reservation.exists())

    def test_abort_before_launch_retains_receipt_and_allows_new_task(self):
        self.reserve()
        self.operation("abort", status="failed", stage="prepare_failed", message="Synthetic preparation failure.", exit_code=1)
        other = self.task_value("2222222222222222")
        self.store.apply("reserve", other["id"], os.getpid(), other)
        self.assertEqual(self.current()["id"], other["id"])
        self.assertTrue((self.store.history / self.task["id"] / "owner.json").is_file())

    def test_two_processes_reserve_exactly_one_task_without_sleep(self):
        context = multiprocessing.get_context("fork")
        barrier, queue = context.Barrier(2), context.Queue()
        children = [context.Process(target=reserve_worker, args=(self.generated["state_program"], str(self.root), str(self.work),
                    self.task_value(suffix), barrier, queue)) for suffix in ("1111111111111111", "2222222222222222")]
        for child in children: child.start()
        try:
            outcomes = sorted([queue.get(timeout=15), queue.get(timeout=15)])
            self.assertEqual(outcomes, ["blocked", "reserved"])
            for child in children:
                child.join(timeout=10)
                self.assertEqual(child.exitcode, 0)
            self.assertTrue(self.store.reservation.is_dir())
            self.assertEqual(self.current()["id"], self.module.load(self.store.reservation / "owner.json")["id"])
        finally:
            for child in children:
                if child.is_alive(): child.terminate()
                child.join(timeout=10)


    def _publication_tree(self):
        records = {}
        for path in sorted([self.root] + list(self.root.rglob("*")), key=str):
            info = path.lstat()
            value = dict(mode=info.st_mode, uid=info.st_uid, gid=info.st_gid,
                         mtime_ns=info.st_mtime_ns, size=info.st_size)
            if stat.S_ISREG(info.st_mode):
                data = path.read_bytes()
                value.update(kind="regular", sha256=hashlib.sha256(data).hexdigest(), bytes_hex=data.hex())
            elif stat.S_ISDIR(info.st_mode):
                value["kind"] = "directory"
            elif stat.S_ISLNK(info.st_mode):
                value.update(kind="symlink", target=os.readlink(str(path)))
            else:
                value["kind"] = "other"
            records[path.relative_to(self.root).as_posix()] = value
        return records

    @contextlib.contextmanager
    def _publication_fault(self, stage, timing, events):
        targets = dict(barrier=self.store.publication, owner=self.store.reservation / "owner.json",
                       job=self.store.job(self.task["id"]) / "state.json", latest=self.store.latest)
        if stage in targets:
            target = targets[stage]
            real_publish = self.module.publish
            real_replace, real_fsync = os.replace, os.fsync

            def publish(path, value):
                if Path(path) != target:
                    return real_publish(path, value)
                if timing == "before":
                    def replace(source, destination):
                        if Path(destination) == target:
                            events.append(dict(stage=stage, point="replace_before_commit", path=str(target)))
                            raise OSError("synthetic publication replace failure")
                        return real_replace(source, destination)
                    with mock.patch.object(self.module.os, "replace", side_effect=replace):
                        return real_publish(path, value)

                def fsync(fd):
                    if stat.S_ISDIR(os.fstat(fd).st_mode):
                        events.append(dict(stage=stage, point="directory_sync_after_replace", path=str(target)))
                        raise OSError("synthetic publication directory sync failure")
                    return real_fsync(fd)
                with mock.patch.object(self.module.os, "fsync", side_effect=fsync):
                    return real_publish(path, value)

            with mock.patch.object(self.module, "publish", side_effect=publish):
                yield
        elif stage in ("retire", "confirm"):
            source = self.store.reservation if stage == "retire" else self.store.publication
            destination = self.store.history / self.task["id"]
            if stage == "confirm":
                destination = destination / "publication-confirmed.json"
            real_rename = os.rename

            def rename(old, new):
                if Path(old) != source or Path(new) != destination:
                    return real_rename(old, new)
                if timing == "after":
                    real_rename(old, new)
                events.append(dict(stage=stage, point=timing + "_rename", source=str(source), destination=str(destination)))
                raise OSError("synthetic publication rename result failure")
            with mock.patch.object(self.module.os, "rename", side_effect=rename):
                yield
        else:
            self.assertIsNone(stage)
            yield

    def _assert_publication_layout(self, expected, before, previous, owner_before, payload, stage):
        barrier, reserved, history, owner_phase, job_terminal, latest_terminal, confirmed = expected
        after = self._publication_tree()
        relative = lambda path: path.relative_to(self.root).as_posix()
        reservation = relative(self.store.reservation)
        old_owner = reservation + "/owner.json"
        history_root = relative(self.store.history)
        receipt_root = history_root + "/" + self.task["id"]
        receipt_owner = receipt_root + "/owner.json"
        receipt_confirmation = receipt_root + "/publication-confirmed.json"
        publication = relative(self.store.publication)
        job = relative(self.store.job(self.task["id"]) / "state.json")
        latest = relative(self.store.latest)
        expected_paths = set(before)
        if history:
            expected_paths.add(history_root)
        if not reserved:
            expected_paths.difference_update((reservation, old_owner))
            expected_paths.update((receipt_root, receipt_owner))
        if barrier:
            expected_paths.add(publication)
        if confirmed:
            expected_paths.add(receipt_confirmation)
        self.assertEqual(set(after), expected_paths, "Unexpected metadata was added, removed or lost")
        self.assertEqual(self.store.reservation.is_dir(), reserved)
        self.assertEqual(self.store.history.is_dir(), history)
        self.assertEqual(self.store.publication.exists(), barrier)
        self.assertEqual((self.store.history / self.task["id"] / "publication-confirmed.json").exists(), confirmed)
        modified_files = set()
        if owner_phase == "ended":
            modified_files.add(old_owner)
        if job_terminal:
            modified_files.add(job)
        if latest_terminal:
            modified_files.add(latest)
        mutable_directories = {"data"}
        if stage == "owner" or owner_phase == "ended":
            mutable_directories.add(reservation)
        if stage in ("job", "latest", "confirm", None):
            mutable_directories.add(relative(self.store.job(self.task["id"])))
        for name, value in before.items():
            if name not in after:
                self.assertIn(name, (reservation, old_owner))
                self.assertFalse(reserved)
            elif value["kind"] == "regular" and name not in modified_files:
                self.assertEqual(after[name], value, "Untouched file changed: " + name)
            elif value["kind"] == "directory":
                for key in ("kind", "mode", "uid", "gid"):
                    self.assertEqual(after[name][key], value[key], "Directory protection changed: " + name)
                if name not in mutable_directories:
                    self.assertEqual(after[name], value, "Unrelated directory changed: " + name)
        for name in set(after) - set(before):
            value = after[name]
            self.assertEqual(value["uid"], os.geteuid())
            self.assertEqual(stat.S_IMODE(value["mode"]), 0o700 if value["kind"] == "directory" else 0o600)
        owner_path = self.store.reservation / "owner.json" if reserved else self.store.history / self.task["id"] / "owner.json"
        self.assertEqual(self.module.load(owner_path), dict(owner_before, phase=owner_phase))
        for name in (old_owner if reserved else receipt_owner, job, latest):
            self.assertEqual(stat.S_IMODE(after[name]["mode"]), 0o600)
            self.assertEqual(after[name]["uid"], os.geteuid())
        if barrier:
            self.assertEqual(self.module.load(self.store.publication), dict(id=self.task["id"]))
        if confirmed:
            self.assertEqual(self.module.load(self.store.history / self.task["id"] / "publication-confirmed.json"), dict(id=self.task["id"]))
        for name, terminal in ((job, job_terminal), (latest, latest_terminal)):
            value = self.module.load(self.root / name)
            if terminal:
                self.store.validate(value)
                self.assertEqual(value["completed_at"], value["updated_at"])
                self.assertEqual(datetime.datetime.strptime(value["updated_at"], "%Y-%m-%dT%H:%M:%SZ").strftime("%Y-%m-%dT%H:%M:%SZ"), value["updated_at"])
                expected_value = dict(previous, **payload)
                expected_value.update(updated_at=value["updated_at"], completed_at=value["updated_at"])
                self.assertEqual(value, expected_value)
            else:
                self.assertEqual(after[name], before[name])
        if job_terminal and latest_terminal:
            self.assertEqual(after[job]["bytes_hex"], after[latest]["bytes_hex"])
        return after

    def _check_publication_case(self, stage, timing, expected):
        events = []
        audit = dict(stage=stage, timing=timing, expected=list(expected), fault_events=events)
        try:
            self.reserve()
            self.operation("claim")
            self.operation("progress", status="restarting", stage="verifying", exit_code=0)
            log = self.store.job(self.task["id"]) / "runner.log"
            log.write_text("synthetic retained runner evidence\n", encoding="utf-8")
            log.chmod(0o600)
            before = self._publication_tree()
            audit["before"] = before
            previous = self.current()
            owner_before = self.module.load(self.store.reservation / "owner.json")
            input_before = json.dumps(self.task, sort_keys=True)
            payload = dict(status="success", stage="success", message="Synthetic completed publication.", exit_code=0, error="")
            with self._publication_fault(stage, timing, events):
                if stage is None:
                    result = self.operation("finish", **payload)
                    self.assertEqual(result, self.current())
                else:
                    error = self.module.CommitUnknown if stage in ("barrier", "owner", "job", "latest") and timing == "after" else OSError
                    with self.assertRaises(error) as caught:
                        self.operation("finish", **payload)
                    audit["observed_error"] = type(caught.exception).__name__
            self.assertEqual(len(events), 0 if stage is None else 1, "The intended failure was not injected exactly once")
            after = self._assert_publication_layout(expected, before, previous, owner_before, payload, stage)
            self.assertEqual(json.dumps(self.task, sort_keys=True), input_before)
            self.assertEqual(self.snapshot(), self.protected)
            reason = "previous publication is unconfirmed" if expected[0] else (
                "previous runner is not confirmed stopped" if expected[5] else "existing task requires confirmation")
            other = self.task_value("2222222222222222")
            with self.assertRaisesRegex(self.module.TaskError, "^" + reason + "$"):
                self.store.apply("reserve", other["id"], os.getpid(), other)
            self.assertEqual(self._publication_tree(), after, "Rejected task admission changed the transaction")
            self.assertEqual(self.snapshot(), self.protected)
            audit.update(outcome="PASS", admission_blocked_by=reason)
        finally:
            pending_error = sys.exc_info()[0] is not None
            try:
                audit["after"] = self._publication_tree()
                audit["protected_before"] = self.protected
                audit["protected_after"] = self.snapshot()
                destination = Path(os.environ["CODEW_UPGRADE_TEST_EVIDENCE"]) / "publication-matrix"
                destination.mkdir(mode=0o700, exist_ok=True)
                with (destination / (self._testMethodName + ".json")).open("x", encoding="utf-8") as output:
                    json.dump(audit, output, ensure_ascii=False, sort_keys=True, indent=2)
            except Exception as evidence_error:
                print("Publication evidence collection failed: " + repr(evidence_error), file=sys.stderr)
                if not pending_error:
                    raise

    def test_publication_01_barrier_replace_failure(self):
        self._check_publication_case("barrier", "before", (False, True, False, "runner", False, False, False))

    def test_publication_02_barrier_directory_sync_unknown(self):
        self._check_publication_case("barrier", "after", (True, True, False, "runner", False, False, False))

    def test_publication_03_owner_replace_failure(self):
        self._check_publication_case("owner", "before", (True, True, False, "runner", False, False, False))

    def test_publication_04_owner_directory_sync_unknown(self):
        self._check_publication_case("owner", "after", (True, True, False, "ended", False, False, False))

    def test_publication_05_reservation_rename_failure(self):
        self._check_publication_case("retire", "before", (True, True, True, "ended", False, False, False))

    def test_publication_06_reservation_moved_then_error(self):
        self._check_publication_case("retire", "after", (True, False, True, "ended", False, False, False))

    def test_publication_07_job_state_replace_failure(self):
        self._check_publication_case("job", "before", (True, False, True, "ended", False, False, False))

    def test_publication_08_job_state_directory_sync_unknown(self):
        self._check_publication_case("job", "after", (True, False, True, "ended", True, False, False))

    def test_publication_09_latest_replace_failure(self):
        self._check_publication_case("latest", "before", (True, False, True, "ended", True, False, False))

    def test_publication_10_latest_directory_sync_unknown(self):
        self._check_publication_case("latest", "after", (True, False, True, "ended", True, True, False))

    def test_publication_11_confirmation_rename_failure(self):
        self._check_publication_case("confirm", "before", (True, False, True, "ended", True, True, False))

    def test_publication_12_confirmation_moved_then_error(self):
        self._check_publication_case("confirm", "after", (False, False, True, "ended", True, True, True))

    def test_publication_13_success_receipt(self):
        self._check_publication_case(None, None, (False, False, True, "ended", True, True, True))


RUNNER_OBSERVATIONS = r'''
import hashlib, json, os, signal, subprocess, sys, time
from pathlib import Path

lab = Path(os.environ['LAB'])
control = json.loads((lab/'runner-control.json').read_text(encoding='utf-8'))
fault = control['fault']
installed = (lab/'installed.json').exists()
name = Path(sys.argv[0]).name
args = sys.argv[1:]

def event(kind, **values):
    line = json.dumps(dict(kind=kind, monotonic_ns=time.monotonic_ns(), **values), sort_keys=True)+'\n'
    fd = os.open(os.environ['TRACE'], os.O_WRONLY|os.O_APPEND)
    try: os.write(fd, line.encode())
    finally: os.close(fd)

def reject():
    event('unexpected-command', command=name, args=args)
    raise SystemExit(93)

def delegate_python(program, raw, state=False, backup=False):
    prefix = ''
    if state and args[4] == 'finish' and json.loads(raw).get('status') == 'success' and fault in ('finish_job', 'finish_job_sync'):
        event('injected-state-fault', fault=fault)
        if fault == 'finish_job':
            prefix = 'import os\n_real_replace=os.replace\ndef _fault(a,b):\n if str(b).endswith("/state.json"): raise OSError("synthetic job publication failure")\n return _real_replace(a,b)\nos.replace=_fault\n'
        else:
            prefix = 'import os\n_real_fsync=os.fsync\ndef _fault(fd):\n if os.path.isdir("/proc/self/fd/"+str(fd)) and os.readlink("/proc/self/fd/"+str(fd)).endswith("/"+'+repr(control['task_id'])+'): raise OSError("synthetic job directory sync failure")\n return _real_fsync(fd)\nos.fsync=_fault\n'
    if backup and fault == 'backup_io':
        event('injected-backup-fault')
        prefix = 'import os\ndef _fault(*a,**k): raise OSError("synthetic backup link failure")\nos.link=_fault\n'
    argv = [control['python']] + args
    if prefix:
        if state:
            argv[2] = prefix+program
        else:
            raw = (prefix+program).encode()
    result = subprocess.run(argv, input=raw, capture_output=True)
    if backup and result.returncode == 0 and fault in ('backup_missing', 'backup_symlink'):
        path = Path(result.stdout.decode().strip())
        if path.parent != Path(control['app'])/'backups' or not path.name.startswith('codew-backup-'):
            reject()
        path.unlink()
        if fault == 'backup_symlink': path.symlink_to(Path(control['app'])/'backups/codew-backup-preserved.tar.gz')
        event('injected-backup-result-race', fault=fault)
    if state:
        value = json.loads(raw)
        current = json.loads((Path(control['app'])/'data/upgrade-task-latest.json').read_text())
        event('state', operation=args[4], requested=value.get('status'), exit=result.returncode,
              status=current['status'], stage=current['stage'])
    if backup: event('backup', exit=result.returncode)
    sys.stdout.buffer.write(result.stdout)
    sys.stderr.buffer.write(result.stderr)
    raise SystemExit(result.returncode)

if name == 'python3':
    if len(args) == 7 and args[0] == '-c' and hashlib.sha256(args[1].encode()).hexdigest() == control['state_sha256']:
        delegate_python(args[1], sys.stdin.buffer.read(), state=True)
    if args and args[0] == '-':
        raw = sys.stdin.buffer.read()
        program = raw.decode('utf-8')
        backup = "meta = dict(format='codew-config-data-v1'" in program
        if 'unsafe ZIP path' in program: event('zip-precheck')
        delegate_python(program, raw, backup=backup)
    os.execv(control['python'], [control['python']]+args)

elif name == 'systemctl':
    event('systemctl', args=args)
    if len(args) == 5 and args[0] == 'show' and args[2] == '-p' and args[4] == '--value':
        unit, prop = args[1], args[3]
        values = {('codew-api','FragmentPath'):str(lab/'system/API_UNIT_FILE'),
                  ('codew-api','EnvironmentFiles'):control['app']+'/.env (ignore_errors=no)',
                  ('codew-api','WorkingDirectory'):control['app'],
                  ('codew-api','ExecStart'):'{ path='+control['app']+'/bin/codew-api-linux-amd64 ; argv[]='+control['app']+'/bin/codew-api-linux-amd64 ; }',
                  ('codew-api','MainPID'):'777',
                  ('xray','ExecStart'):''}
        if prop == 'LoadState' and unit in ('zxy-panel-api','zxy-agent','zxy-panel','codew-agent','xray'):
            value = 'not-found'
            if installed and fault == 'xray_missing' and unit == 'codew-agent': value = 'loaded'
        elif prop in ('StartLimitAction','FailureAction','SuccessAction'):
            value = 'none'
        elif prop in ('OnFailure','OnSuccess','DropInPaths','UnsetEnvironment','ExecStartPre','ExecStartPost','ExecStop','ExecStopPost','ExecReload'):
            value = ''
        elif (unit, prop) in values:
            value = values[(unit, prop)]
        else: reject()
        if fault == 'foreign_unit' and (unit,prop) == ('codew-api','FragmentPath'):
            value = str(lab/'system/foreign.service')
        print(value)
    elif args and args[0] == 'is-active' and args[-1] in ('codew-api','nginx','codew-agent'):
        if args[-1] == 'codew-agent' and not (installed and fault == 'xray_missing'): reject()
        if installed and fault == 'api_inactive' and args[-1] == 'codew-api':
            if '--quiet' not in args: print('inactive')
            raise SystemExit(3)
        if '--quiet' not in args: print('active')
    else: reject()

elif name == 'nginx':
    if args != ['-t']: reject()
    event('nginx')
    if installed and fault == 'nginx_invalid': raise SystemExit(1)

elif name == 'curl':
    if args and args[-1] == control['download_url']:
        expected = ['--fail','--location','--silent','--show-error','--proto','=https','--proto-redir','=https',
                    '--connect-timeout','10','--max-time','300','-o',control['package'],control['download_url']]
        if args != expected or Path.cwd() != Path(control['work'])/control['task_id']: reject()
        event('download', url=args[-1])
        if fault == 'download_failed': raise SystemExit(28)
        if fault == 'gate':
            fd = int(os.environ['RUNNER_GATE_FD'])
            os.write(fd,b'download\n')
            if os.read(fd,1) != b'x': reject()
        Path(args[-2]).write_bytes(Path(control['archive']).read_bytes())
    else:
        if args[:-1] != ['-fsS','--connect-timeout','2','--max-time','4','--max-filesize','65536']: reject()
        urls = control['health_urls']
        if not args or args[-1] not in urls: reject()
        index = urls.index(args[-1])
        event('health', route=index)
        if not installed: reject()
        if fault == 'health_timeout': raise SystemExit(28)
        if fault == 'html': print('<html>synthetic SPA, not health</html>')
        elif fault == 'invalid_json': print('{invalid')
        elif fault == 'root_proxy' and index == 1: print('{}')
        elif fault == 'prefixed_proxy' and index == 2: print('{}')
        else:
            value = dict(status='ok', service='codew-api', version=control['target'])
            if fault == 'wrong_service': value['service']='foreign-api'
            if fault == 'wrong_health_version': value['version']=control['current']
            print(json.dumps(value))

elif name == 'unzip':
    if args != ['-q',control['package']] or Path.cwd() != Path(control['work'])/control['task_id']: reject()
    event('extract')
    os.execv(control['unzip'], [control['unzip']]+args)

elif name == 'sleep':
    if args != ['1']: reject()
    event('poll-interval')

elif name == 'synthetic-installer.py':
    event('installer')
    expected = dict(APP_DIR=control['app'], CONFIG_DIR=control['config'], CODEW_INSTALL_MODE='fast',
                    FRESH_INSTALL='false', CODEW_UPDATE_MANIFEST_URL=control['manifest_url'])
    actual = {key:os.environ.get(key) for key in expected}
    (lab/'installer-environment.json').write_text(json.dumps(actual,sort_keys=True))
    if actual != expected: reject()
    if fault == 'installer_failed': raise SystemExit(19)
    version = control['current'] if fault == 'metadata_version' else control['target']
    (Path(control['app'])/'VERSION').write_text(version+'\n')
    panel = Path(control['config'])/'panel.info'
    lines = panel.read_text().splitlines()
    panel.write_text('\n'.join('VERSION='+version if line.startswith('VERSION=') else line for line in lines)+'\n')
    if fault in ('agent_missing','xray_missing'):
        environment = Path(control['app'])/'.env'
        text = environment.read_text().replace('CODEW_AUTO_AGENT=false\n','CODEW_AUTO_AGENT=true\n')
        if fault == 'xray_missing': text += 'CODEW_SETUP_XRAY_SERVICE=true\n'
        environment.write_text(text)
    (lab/'installed.json').write_text(json.dumps(dict(version=version)))

else:
    reject()
'''


class UpgradeRunnerExecution(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.generated = generated_fixture()
        cls.binary = Path(os.environ['CODEW_UPGRADE_RUNNER_BINARY'])
        if not cls.binary.is_file() or cls.binary.is_symlink():
            raise RuntimeError('A verified Linux Go test binary is required')
        path = Path(__file__).with_name('cli_reliability_test.py')
        spec = importlib.util.spec_from_file_location('codew_cli_runner_fixtures', path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        cls.fixture_class = module.CliReliability

    def setUp(self):
        self.fixture = self.fixture_class('test_health_requires_exact_service_version_and_json')
        self.fixture.setUp()
        self.base, self.app, self.config = self.fixture.root, self.fixture.app, self.fixture.config
        self.work = self.base/'runner-work'
        self.work.mkdir(mode=0o700)
        self.out = Path(os.environ['CODEW_UPGRADE_TEST_EVIDENCE'])/'runner-cases'/self._testMethodName
        self.out.mkdir(parents=True)
        self.processes = []
        self.gate_parent = self.gate_child = None
        self.addCleanup(self.cleanup_fixture)
        self.fixture.restore_setup()
        self.fixture.data.write_text(json.dumps(dict(servers={'synthetic':{'id':'synthetic'}},
            nodes={'node':{'id':'node','server_id':'synthetic'}},
            clients={'client':{'node_ids':['node'],'relay_route_ids':['relay']}},
            relay_routes={'relay':{'landing_node_id':'node'}},logs=['synthetic retained log'])))
        (self.app/'logs').mkdir()
        (self.app/'logs/previous.log').write_text('synthetic prior business log\n')
        (self.app/'backups').mkdir()
        (self.app/'backups/codew-backup-preserved.tar.gz').write_bytes(b'synthetic preserved old backup')
        for name in ('BBR_CONF_FILE','BBR_DISABLED_FILE'):
            (self.fixture.system/name).write_text('synthetic preserved '+name+'\n')
        self.protected = self.fixture.snapshot(backups=True)
        self.control = dict(fault='', python=sys.executable, unzip=shutil.which('unzip'), app=str(self.app),
            config=str(self.config), work=str(self.work), archive=str(self.base/'download.zip'),
            task_id=self.generated['task']['id'], package=self.generated['manifest']['package'],
            current='0.7.8-stable-engineering',target=self.generated['manifest']['latest'],
            download_url=self.generated['manifest']['download_url'],
            manifest_url='https://manifest.invalid/version.json?channel=test&v=1',
            health_urls=['http://127.0.0.1:18088/api/health','http://127.0.0.1:49321/api/health',
                         'http://127.0.0.1:49321/fixturebase/api/health'],
            state_sha256=hashlib.sha256(self.generated['state_program'].encode()).hexdigest())
        self.assertIsNotNone(self.control['unzip'], 'Existing unzip is required; do not install tools')
        helper = self.base/'command-observation.py'
        helper.write_text('#!'+sys.executable+'\n'+RUNNER_OBSERVATIONS,encoding='utf-8')
        helper.chmod(0o700)
        (self.base/'synthetic-installer.py').symlink_to(helper)
        for name in ('systemctl','systemd-run','nginx','curl','unzip','sleep','python3'):
            path = self.fixture.bin/name
            if path.is_symlink(): path.unlink()
            self.assertFalse(path.exists())
            path.symlink_to(helper)
        self.write_control()
        self.env = dict(self.fixture.env, RUNNER_LAB=str(self.base))
        self.before = self.full_tree()

    def cleanup_fixture(self):
        for process in self.processes:
            if process.poll() is None:
                os.killpg(process.pid,signal.SIGKILL)
            process.wait(timeout=10)
        for channel in (self.gate_parent,self.gate_child):
            if channel is not None: channel.close()
        if hasattr(self, 'out'):
            records = dict(before=getattr(self,'before',{}),after=self.full_tree(),events=self.events())
            (self.out/'full-evidence.json').write_text(json.dumps(records,indent=2,sort_keys=True),encoding='utf-8')
            with tarfile.open(self.out/'synthetic-fixture.tar.gz','w:gz') as archive:
                archive.add(self.base,arcname='fixture',recursive=True)
        result = self._outcome.result
        if any(test is self for test,_ in result.failures+result.errors):
            self.fixture.temp._finalizer.detach()
            (self.out/'retained-native-fixture.txt').write_text(str(self.base)+'\n')
        else:
            self.fixture.doCleanups()

    def full_tree(self):
        result = {}
        for path in sorted(self.base.rglob('*')):
            info = path.lstat()
            value = dict(mode=info.st_mode,uid=info.st_uid,gid=info.st_gid,mtime_ns=info.st_mtime_ns)
            if stat.S_ISREG(info.st_mode):
                data = path.read_bytes()
                value.update(sha256=hashlib.sha256(data).hexdigest(),bytes_b64=base64.b64encode(data).decode())
            elif stat.S_ISLNK(info.st_mode): value['link']=os.readlink(path)
            result[str(path.relative_to(self.base))]=value
        return result

    def write_control(self):
        path = self.base/'runner-control.json'
        path.write_text(json.dumps(self.control,sort_keys=True),encoding='utf-8')
        path.chmod(0o600)

    def events(self, kind=None):
        records = [json.loads(line) for line in self.fixture.trace.read_text(encoding='utf-8').splitlines()]
        return records if kind is None else [row for row in records if row['kind']==kind]

    def state_entry(self, operation, payload, task_id=None, raw=None):
        argv = [sys.executable,'-c',self.generated['state_program'],str(self.app),str(self.work),
                operation,task_id or self.task['id'],str(os.getpid())]
        result = subprocess.run(argv,input=raw if raw is not None else json.dumps(payload).encode(),
                                capture_output=True,timeout=15,env=self.env)
        target = self.out/('entry-'+str(len(list(self.out.glob('entry-*.json'))))+'.json')
        target.write_text(json.dumps(dict(operation=operation,exit=result.returncode,
            stdout=result.stdout.decode('utf-8'),stderr=result.stderr.decode('utf-8')),indent=2),encoding='utf-8')
        return result

    def prepare(self, fault='', member=None):
        self.control['fault']=fault
        package = Path(self.control['archive'])
        with zipfile.ZipFile(package,'w',compression=zipfile.ZIP_DEFLATED) as archive:
            if fault != 'missing_installer':
                archive.writestr(self.control['package'][:-4]+'/deploy/install.sh',
                    '#!/usr/bin/env bash\nset -euo pipefail\npython3 "$LAB/synthetic-installer.py"\n')
            if member:
                if fault == 'zip_symlink':
                    info = zipfile.ZipInfo(member)
                    info.create_system=3
                    info.external_attr=(stat.S_IFLNK|0o777)<<16
                    archive.writestr(info,'../../synthetic-outside')
                else: archive.writestr(member,'synthetic unsafe member')
            if fault == 'missing_installer': archive.writestr('codew-v0.7.9-next/README','synthetic')
        spec = dict(sandbox=str(self.base),app=str(self.app),config=str(self.config),work=str(self.work),
                    cli=str(self.fixture.cli),archive=str(package))
        source = self.base/'runner-input.json'
        source.write_text(json.dumps(spec),encoding='utf-8')
        generated = self.out/'generated'
        generated.mkdir(mode=0o700)
        env = dict(self.env,TMPDIR=str(self.base.parent),CODEW_UPGRADE_RUNNER_INPUT=str(source),
                   CODEW_UPGRADE_TEST_EVIDENCE=str(generated))
        result = subprocess.run([str(self.binary),'-test.run=^TestUpgradeStateProgramFixture$',
            '-test.count=1','-test.v'],env=env,capture_output=True,timeout=20)
        (self.out/'go-export.stdout').write_bytes(result.stdout)
        (self.out/'go-export.stderr').write_bytes(result.stderr)
        self.assertEqual(result.returncode,0,result.stderr.decode())
        self.generated = json.loads((generated/'state-fixture.json').read_text(encoding='utf-8'))
        self.task = self.generated['task']
        self.assertEqual(self.generated['source_os'],'linux')
        self.assertEqual(self.task['sha256'],hashlib.sha256(package.read_bytes()).hexdigest())
        self.assertEqual(self.task['log_file'],str(self.app/'logs/upgrades'/self.task['id']/'runner.log'))
        self.assertEqual(self.task['work_dir'],str(self.work/self.task['id']))
        self.assertEqual(self.generated['cli'],str(self.fixture.cli))
        self.assertEqual(self.generated['runtime']['config_dir'],str(self.config))
        self.assertEqual(self.control['state_sha256'],hashlib.sha256(self.generated['state_program'].encode()).hexdigest())
        self.control.update(task_id=self.task['id'],download_url=self.task['download_url'])
        self.write_control()
        result = self.state_entry('reserve',self.task)
        self.assertEqual(result.returncode,0,result.stderr.decode())
        self.log = Path(self.task['log_file'])
        with self.log.open('xb') as output: output.write(b'')
        self.log.chmod(0o600)
        self.runner = self.log.with_name('runner.sh')
        self.runner.write_bytes(self.generated['runner'].encode())
        self.runner.chmod(0o700)
        if fault == 'sha_mismatch': package.write_bytes(package.read_bytes()+b'synthetic hash mismatch')
        if fault == 'cli_missing': self.fixture.cli.rename(self.base/'retained-cli.sh')
        if fault == 'cli_symlink':
            retained=self.base/'retained-cli.sh'
            self.fixture.cli.rename(retained)
            self.fixture.cli.symlink_to(retained)
        if fault == 'legacy':
            (self.base/'legacy').mkdir()
            (self.base/'legacy/0').write_text('synthetic legacy installation; do not touch')
        if fault == 'installed_version_changed':
            (self.app/'VERSION').write_text('0.7.7.5-stability-polish-agent-xray\n')
        self.protected = self.fixture.snapshot(backups=True)
        self.before = self.full_tree()

    def launch(self):
        process = subprocess.Popen(['/bin/bash','--noprofile','--norc',str(self.runner)],env=self.env,
            stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True,
            pass_fds=() if self.gate_child is None else (self.gate_child.fileno(),))
        self.processes.append(process)
        return process

    def complete(self, process):
        stdout,stderr=process.communicate(timeout=20)
        index=self.processes.index(process)
        (self.out/('runner-'+str(index)+'.stdout')).write_bytes(stdout)
        (self.out/('runner-'+str(index)+'.stderr')).write_bytes(stderr)
        (self.out/('runner-'+str(index)+'.exit')).write_text(str(process.returncode)+'\n')
        self.assertNotIn(b'command not found',self.log.read_bytes()+stderr)
        self.assertEqual(self.events('unexpected-command'),[])
        return process.returncode

    def current(self):
        return json.loads((self.app/'data/upgrade-task-latest.json').read_text())

    def assert_task_outputs(self, latest_status, job_status):
        data = self.app/'data'
        latest_path = data/'upgrade-task-latest.json'
        job_path = self.log.with_name('state.json')
        live_owner = data/'.upgrade-reservation/owner.json'
        archived = data/'.upgrade-reservations'/self.task['id']
        archived_owner = archived/'owner.json'
        confirmed = archived/'publication-confirmed.json'
        barrier = data/'.upgrade-publication'

        def private_bytes(path):
            info = path.lstat()
            self.assertTrue(stat.S_ISREG(info.st_mode), str(path))
            self.assertEqual(stat.S_IMODE(info.st_mode), 0o600, str(path))
            self.assertEqual(info.st_uid, os.geteuid(), str(path))
            return path.read_bytes()

        def before_bytes(path):
            return base64.b64decode(self.before[str(path.relative_to(self.base))]['bytes_b64'])

        mutable = {'status', 'stage', 'message', 'updated_at', 'completed_at', 'exit_code', 'error'}
        immutable = {key: value for key, value in self.task.items() if key not in mutable}
        states = []
        for path, expected in ((latest_path, latest_status), (job_path, job_status)):
            value = json.loads(private_bytes(path))
            self.assertEqual(value['status'], expected, str(path))
            self.assertEqual({key: field for key, field in value.items() if key not in mutable},
                             immutable, str(path))
            self.assertEqual(bool(value.get('completed_at')), expected in ('success', 'failed'), str(path))
            states.append(value)
        if latest_status == job_status:
            self.assertEqual(states[0], states[1])

        active = latest_status == 'downloading'
        owner_path, absent_owner = (live_owner, archived_owner) if active else (archived_owner, live_owner)
        owner = json.loads(private_bytes(owner_path))
        prior_owner = json.loads(before_bytes(live_owner))
        self.assertEqual(owner['id'], self.task['id'])
        self.assertEqual(owner['creator'], prior_owner['creator'])
        self.assertEqual(owner['phase'], 'runner' if active else 'ended')
        boot, pid, start = owner['runner'].split(':')
        self.assertEqual(boot, prior_owner['creator'].split(':', 1)[0])
        self.assertEqual(pid, str(self.processes[0].pid))
        self.assertTrue(start.isdecimal())
        self.assertFalse(os.path.lexists(absent_owner), str(absent_owner))

        for path, expected in ((confirmed, latest_status in ('success', 'failed')),
                               (barrier, latest_status == 'restarting')):
            if expected:
                self.assertEqual(json.loads(private_bytes(path)), {'id': self.task['id']})
            else:
                self.assertFalse(os.path.lexists(path), str(path))
        self.assertTrue(private_bytes(self.log).startswith(before_bytes(self.log)), 'Runner log is append-only')
        return {str(path.relative_to(self.base)) for path in (latest_path, job_path, live_owner, self.log)}

    def assert_protected(self, installed=False, *, latest_status=None, job_status=None):
        after=self.fixture.snapshot(backups=True)
        allowed={'app/VERSION','config/panel.info'} if installed else set()
        if installed and self.control['fault'] in ('agent_missing','xray_missing'): allowed.add('app/.env')
        if latest_status is not None:
            allowed.update(self.assert_task_outputs(latest_status, job_status or latest_status))
        for path,value in self.protected.items():
            if path not in allowed: self.assertEqual(after.get(path),value,'Protected synthetic file changed: '+path)

    def assert_backup(self, exists=True):
        paths=[p for p in (self.app/'backups').iterdir() if p.name != 'codew-backup-preserved.tar.gz']
        if not exists:
            self.assertEqual(paths,[])
            return
        self.assertEqual(len(paths),1)
        archive=paths[0]
        self.assertFalse(archive.is_symlink())
        self.assertEqual(stat.S_IMODE(archive.stat().st_mode),0o600)
        with tarfile.open(archive,'r:gz') as source:
            manifest=json.load(source.extractfile('manifest.json'))
            self.assertEqual(manifest['format'],'codew-config-data-v1')
            self.assertEqual(manifest['reason'],'pre-upgrade')
            self.assertEqual(manifest['install_dir'],str(self.app))
            self.assertEqual(manifest['config_dir'],str(self.config))
            for key,path in (('database','app/data/codew.json'),('environment','app/.env'),
                             ('panel_info','config/panel.info'),('bbr','system/BBR_CONF_FILE'),
                             ('bbr_disabled','system/BBR_DISABLED_FILE')):
                data=source.extractfile('files/'+key).read()
                self.assertEqual(hashlib.sha256(data).hexdigest(),self.protected[path][0])
                self.assertEqual(manifest['files'][key]['mtime_ns'],self.protected[path][1])

    def assert_failed(self, code=None, installed=False):
        result=self.complete(self.launch())
        self.assertNotEqual(result,0)
        if code is not None: self.assertEqual(result,code)
        state=self.current()
        self.assertEqual(state['status'],'failed')
        self.assertEqual(state['exit_code'],result)
        self.assertTrue(state['completed_at'])
        history=self.app/'data/.upgrade-reservations'/self.task['id']
        self.assertEqual(json.loads((history/'owner.json').read_text())['phase'],'ended')
        self.assertTrue((history/'publication-confirmed.json').is_file())
        self.assertFalse((self.app/'data/.upgrade-reservation').exists())
        self.assertEqual(sum(row.get('requested')=='success' and row['exit']==0 for row in self.events('state')),0)
        self.assertLessEqual(len(self.events('download')),1)
        self.assertLessEqual(len(self.events('extract')),1)
        self.assertLessEqual(len(self.events('installer')),1)
        self.assert_protected(installed=installed, latest_status='failed')
        return result

    def rejected_before_install(self, fault, *, downloaded=False, member=None, backup=False):
        self.prepare(fault,member=member)
        self.assert_failed()
        self.assertEqual(len(self.events('download')),int(downloaded))
        self.assertEqual(self.events('extract'),[])
        self.assertEqual(self.events('installer'),[])
        self.assertEqual(len(self.events('backup')),int(backup))
        if not backup or fault == 'backup_io': self.assert_backup(False)
        self.assertFalse((self.work/self.task['id']/self.task['package'][:-4]).exists())

    def test_runner_01_success_real_state_backup_zip_and_literal_arguments(self):
        self.prepare()
        self.assertEqual(self.complete(self.launch()),0)
        state=self.current()
        self.assertEqual(state['status'],'success')
        self.assertEqual(state['stage'],'success')
        self.assertEqual(state['exit_code'],0)
        self.assertTrue(state['completed_at'])
        self.assertEqual([row['operation'] for row in self.events('state')],['claim']+['progress']*5+['finish'])
        self.assertEqual([row['kind'] for row in self.events() if row['kind'] in
            ('download','zip-precheck','backup','extract','installer','nginx','health')],
            ['nginx','download','zip-precheck','backup','extract','installer','nginx','health','health','health'])
        self.assertEqual([row['route'] for row in self.events('health')],[0,1,2])
        self.assertEqual(self.events('download')[0]['url'],self.control['download_url'])
        self.assertEqual(json.loads((self.base/'installer-environment.json').read_text()),dict(
            APP_DIR=str(self.app),CONFIG_DIR=str(self.config),CODEW_INSTALL_MODE='fast',FRESH_INSTALL='false',
            CODEW_UPDATE_MANIFEST_URL=self.control['manifest_url']))
        self.assertEqual(self.events('poll-interval'),[])
        self.assert_backup()
        self.assert_protected(installed=True, latest_status='success')
        self.assertFalse((self.app/'data/.upgrade-reservation').exists())
        receipt=self.app/'data/.upgrade-reservations'/self.task['id']/'publication-confirmed.json'
        self.assertTrue(receipt.is_file())
        other=dict(self.task,id='upgrade_20261117_120000_2222222222222222')
        other.update(log_file=str(self.app/'logs/upgrades'/other['id']/'runner.log'),work_dir=str(self.work/other['id']))
        self.assertEqual(self.state_entry('reserve',other,task_id=other['id']).returncode,0)

    def test_runner_02_missing_cli_stops_before_download(self): self.rejected_before_install('cli_missing')
    def test_runner_03_linked_cli_stops_before_download(self): self.rejected_before_install('cli_symlink')
    def test_runner_04_legacy_namespace_stops_before_download(self): self.rejected_before_install('legacy')
    def test_runner_05_changed_installed_version_stops_before_download(self): self.rejected_before_install('installed_version_changed')
    def test_runner_06_foreign_api_unit_stops_before_download(self): self.rejected_before_install('foreign_unit')
    def test_runner_07_download_failure_never_extracts(self): self.rejected_before_install('download_failed',downloaded=True)
    def test_runner_08_sha_mismatch_never_extracts(self): self.rejected_before_install('sha_mismatch',downloaded=True)
    def test_runner_09_matching_sha_traversal_zip_is_rejected(self): self.rejected_before_install('zip_path',downloaded=True,member='../synthetic-outside')
    def test_runner_10_matching_sha_linked_zip_is_rejected(self): self.rejected_before_install('zip_symlink',downloaded=True,member='codew-v0.7.9-next/link')
    def test_runner_11_matching_sha_missing_installer_is_rejected(self): self.rejected_before_install('missing_installer',downloaded=True)
    def test_runner_12_backup_io_failure_stops_before_extract(self): self.rejected_before_install('backup_io',downloaded=True,backup=True)
    def test_runner_13_disappeared_backup_stops_before_extract(self): self.rejected_before_install('backup_missing',downloaded=True,backup=True)
    def test_runner_14_linked_backup_stops_before_extract(self): self.rejected_before_install('backup_symlink',downloaded=True,backup=True)

    def test_runner_15_installer_nonzero_exit_is_not_hidden(self):
        self.prepare('installer_failed')
        self.assert_failed(19)
        self.assertEqual(len(self.events('installer')),1)
        self.assertEqual(self.events('health'),[])
        self.assert_backup()

    def verification_failure(self, fault, route=None):
        self.prepare(fault)
        self.assert_failed(34 if route is not None else None,installed=True)
        self.assertEqual(len(self.events('installer')),1)
        self.assert_backup()
        if route is not None:
            self.assertEqual([row['route'] for row in self.events('health')],list(range(route+1))*30)
            self.assertEqual(len(self.events('poll-interval')),30)
        else: self.assertEqual(self.events('health'),[])

    def test_runner_16_installer_zero_with_wrong_metadata_is_not_success(self): self.verification_failure('metadata_version')
    def test_runner_17_invalid_nginx_is_not_success(self): self.verification_failure('nginx_invalid')
    def test_runner_18_inactive_api_is_not_success(self): self.verification_failure('api_inactive')
    def test_runner_19_required_agent_missing_is_not_success(self): self.verification_failure('agent_missing')
    def test_runner_20_required_xray_missing_is_not_success(self): self.verification_failure('xray_missing')
    def test_runner_21_html_is_not_health(self): self.verification_failure('html',0)
    def test_runner_22_invalid_json_is_not_health(self): self.verification_failure('invalid_json',0)
    def test_runner_23_wrong_service_is_not_health(self): self.verification_failure('wrong_service',0)
    def test_runner_24_wrong_version_is_not_health(self): self.verification_failure('wrong_health_version',0)
    def test_runner_25_root_proxy_health_must_pass(self): self.verification_failure('root_proxy',1)
    def test_runner_26_prefixed_proxy_health_must_pass(self): self.verification_failure('prefixed_proxy',2)
    def test_runner_27_health_timeout_is_not_success(self): self.verification_failure('health_timeout',0)

    def gated(self):
        self.prepare('gate')
        self.gate_parent,self.gate_child=socket.socketpair(type=socket.SOCK_DGRAM)
        self.gate_parent.settimeout(10)
        self.env['RUNNER_GATE_FD']=str(self.gate_child.fileno())
        process=self.launch()
        self.assertEqual(self.gate_parent.recv(64),b'download\n')
        self.assertEqual(self.current()['status'],'downloading')
        return process

    def test_runner_28_duplicate_claim_cannot_replace_live_runner(self):
        first=self.gated()
        before=self.current()
        owner=(self.app/'data/.upgrade-reservation/owner.json').read_bytes()
        self.assertNotEqual(self.complete(self.launch()),0)
        self.assertEqual(self.current(),before)
        self.assertEqual((self.app/'data/.upgrade-reservation/owner.json').read_bytes(),owner)
        self.assertEqual(len(self.events('download')),1)
        self.assertEqual(self.gate_parent.send(b'x'),1)
        self.assertEqual(self.complete(first),0)
        self.assertEqual(self.current()['status'],'success')
        self.assertEqual(len(self.events('installer')),1)
        self.assert_backup()
        self.assert_protected(installed=True, latest_status='success')

    def interrupted(self, signum, expected):
        process=self.gated()
        before=self.current()
        os.killpg(process.pid,signum)
        self.assertEqual(self.complete(process),expected)
        self.assertEqual(self.events('extract'),[])
        self.assertEqual(self.events('installer'),[])
        self.assertEqual(self.events('backup'),[])
        self.assert_protected(latest_status='downloading' if signum == signal.SIGKILL else 'failed')
        if signum == signal.SIGKILL:
            self.assertEqual(self.current(),before)
            self.assertTrue((self.app/'data/.upgrade-reservation/owner.json').is_file())
            result=self.state_entry('reserve',self.task)
            self.assertNotEqual(result.returncode,0)
        else:
            self.assertEqual(self.current()['status'],'failed')
            self.assertEqual(self.current()['stage'],'interrupted')
            self.assertEqual(self.current()['exit_code'],expected)

    def test_runner_29_term_keeps_evidence_and_reports_interruption(self): self.interrupted(signal.SIGTERM,143)
    def test_runner_30_hup_keeps_evidence_and_reports_interruption(self): self.interrupted(signal.SIGHUP,143)
    def test_runner_31_int_keeps_evidence_and_reports_interruption(self): self.interrupted(signal.SIGINT,130)
    def test_runner_32_kill_retains_unconfirmed_reservation(self): self.interrupted(signal.SIGKILL,-signal.SIGKILL)

    def unconfirmed_finish(self, fault, job_status):
        self.prepare(fault)
        self.assertNotEqual(self.complete(self.launch()),0)
        self.assertEqual(len(self.events('injected-state-fault')),1)
        self.assertNotEqual(self.current()['status'],'success')
        self.assertEqual(self.current()['status'],'restarting')
        job=json.loads(self.log.with_name('state.json').read_text())
        self.assertEqual(job['status'],job_status)
        self.assertTrue((self.app/'data/.upgrade-publication').is_file())
        self.assertFalse((self.app/'data/.upgrade-reservation').exists())
        receipt=self.app/'data/.upgrade-reservations'/self.task['id']
        self.assertEqual(json.loads((receipt/'owner.json').read_text())['phase'],'ended')
        self.assertFalse((receipt/'publication-confirmed.json').exists())
        result=self.state_entry('reserve',self.task)
        self.assertNotEqual(result.returncode,0)
        self.assertEqual(self.current()['status'],'restarting')
        self.assert_backup()
        self.assert_protected(installed=True, latest_status='restarting', job_status=job_status)

    def test_runner_33_finish_job_publish_failure_retains_barrier(self): self.unconfirmed_finish('finish_job','restarting')
    def test_runner_34_finish_job_sync_failure_retains_barrier(self): self.unconfirmed_finish('finish_job_sync','success')

    def test_runner_35_real_state_stdin_rejects_bad_json_without_writes(self):
        self.prepare()
        before=self.full_tree()
        for raw in (b'{broken',b'[]',b'{} {}',b'x'*1048577):
            result=self.state_entry('progress',{},raw=raw)
            self.assertNotEqual(result.returncode,0)
            value=json.loads(result.stdout)
            self.assertIs(value['ok'],False)
            self.assertEqual(value['error'],'Task state operation failed or is unconfirmed; reservation and evidence must be retained.')
            self.assertEqual(self.full_tree(),before)
        self.assert_protected()


if __name__ == "__main__":
    unittest.main(verbosity=2, failfast=True)
