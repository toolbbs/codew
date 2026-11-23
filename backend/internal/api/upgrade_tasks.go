// SPDX-License-Identifier: AGPL-3.0-only
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type upgradeTaskState struct {
	ID             string         `json:"id"`
	Status         string         `json:"status"`
	Stage          string         `json:"stage"`
	Message        string         `json:"message"`
	CurrentVersion string         `json:"current_version"`
	TargetVersion  string         `json:"target_version"`
	Package        string         `json:"package"`
	DownloadURL    string         `json:"download_url"`
	SHA256         string         `json:"sha256"`
	LogFile        string         `json:"log_file"`
	WorkDir        string         `json:"work_dir"`
	StartedAt      string         `json:"started_at"`
	UpdatedAt      string         `json:"updated_at"`
	CompletedAt    string         `json:"completed_at,omitempty"`
	ExitCode       int            `json:"exit_code,omitempty"`
	Error          string         `json:"error,omitempty"`
	Manifest       updateManifest `json:"manifest,omitempty"`
	Stale          bool           `json:"stale,omitempty"`
	StaleReason    string         `json:"stale_reason,omitempty"`
}

// Both the API and the detached runner use this one Linux state transaction.
const upgradeStateProgram = `import contextlib, datetime, fcntl, json, os, re, signal, stat, sys, tempfile
from pathlib import Path

ACTIVE = {'pending':0, 'queued':0, 'running':1, 'downloading':2, 'verifying':3, 'backing_up':4, 'installing':5, 'restarting':6}
ID = re.compile(r'^upgrade_[0-9]{8}_[0-9]{6}_[0-9a-f]{16}$')

class TaskError(Exception): pass
class CommitUnknown(TaskError): pass

def safe(path):
    path = Path(path)
    if not path.is_absolute() or '..' in path.parts or any(p.is_symlink() for p in (path,) + tuple(path.parents)):
        raise TaskError('unsafe task path')
    return path

def load(path):
    path = safe(path)
    try: fd = os.open(str(path), os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except FileNotFoundError: return None
    with os.fdopen(fd, 'rb') as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077 or info.st_size > 1048576:
            raise TaskError('task metadata is not private or regular')
        value = json.loads(source.read(1048577).decode('utf-8'))
    if not isinstance(value, dict): raise TaskError('invalid task metadata')
    return value

def publish(path, value):
    path = safe(path)
    if path.exists(): load(path)
    temporary = None
    committed = False
    try:
        fd, temporary = tempfile.mkstemp(prefix='.upgrade-state-', dir=str(path.parent))
        with os.fdopen(fd, 'w', encoding='utf-8') as output:
            json.dump(value, output, ensure_ascii=False, sort_keys=True)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, str(path))
        committed = True
        temporary = None
        directory = os.open(str(path.parent), os.O_RDONLY | os.O_DIRECTORY)
        try: os.fsync(directory)
        finally: os.close(directory)
    except OSError:
        if committed: raise CommitUnknown('state publication confirmation failed')
        raise
    finally:
        if temporary and os.path.exists(temporary): os.unlink(temporary)

def process_mark(pid):
    pid = str(pid)
    if not re.fullmatch(r'[1-9][0-9]*', pid): raise TaskError('invalid task process')
    try:
        fields = (Path('/proc') / pid / 'stat').read_text().rsplit(') ',1)[1].split()
        boot = Path('/proc/sys/kernel/random/boot_id').read_text().strip()
        return boot + ':' + pid + ':' + fields[19]
    except FileNotFoundError: return None

def alive(mark):
    if not isinstance(mark, str) or len(mark.split(':')) != 3: raise TaskError('invalid process receipt')
    return process_mark(mark.split(':')[1]) == mark

class TaskState:
    def __init__(self, root, work_root):
        self.root, self.work_root = safe(root), safe(work_root)
        self.data = safe(self.root / 'data')
        self.latest = self.data / 'upgrade-task-latest.json'
        self.reservation = self.data / '.upgrade-reservation'
        self.history = self.data / '.upgrade-reservations'
        self.publication = safe(self.data / '.upgrade-publication')

    def job(self, task_id):
        if not isinstance(task_id,str) or not ID.fullmatch(task_id): raise TaskError('invalid task ID')
        return safe(self.root / 'logs' / 'upgrades' / task_id)

    def validate(self, value):
        if not isinstance(value,dict): raise TaskError('invalid task state')
        if any(not isinstance(value.get(key),str) or not value.get(key) for key in ('id','status','stage','current_version','target_version','package','download_url','sha256','log_file','work_dir','started_at','updated_at')): raise TaskError('invalid task fields')
        task_id = value.get('id')
        directory = self.job(task_id)
        manifest = value.get('manifest')
        if value.get('status') not in set(ACTIVE) | {'success','failed'} or not isinstance(manifest,dict): raise TaskError('invalid task state')
        if value.get('log_file') != str(directory/'runner.log') or value.get('work_dir') != str(self.work_root/task_id): raise TaskError('task path mismatch')
        if any(value.get(key) != manifest.get(other) for key,other in [('target_version','latest'),('package','package'),('download_url','download_url'),('sha256','sha256')]): raise TaskError('task manifest mismatch')
        package = value.get('package','')
        if not re.fullmatch(r'codew-v[0-9]+(?:\.[0-9]+)*-[A-Za-z0-9_-]+\.zip',package) or not re.fullmatch(r'[0-9a-fA-F]{64}',value.get('sha256','')): raise TaskError('invalid task artifact')
        return value

    @contextlib.contextmanager
    def lock(self):
        safe(self.root)
        if not self.root.is_dir(): raise TaskError('installation root is unavailable')
        self.data.mkdir(mode=0o755,exist_ok=True)
        safe(self.data)
        path = safe(self.data/'.upgrade-state.lock')
        fd = os.open(str(path), os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        previous = signal.getsignal(signal.SIGALRM)
        def timeout(signum, frame): raise TaskError('task coordination timed out')
        try:
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077: raise TaskError('invalid coordination file')
            signal.signal(signal.SIGALRM,timeout)
            signal.alarm(10)
            fcntl.flock(fd,fcntl.LOCK_EX)
            yield
        finally:
            signal.alarm(0)
            signal.signal(signal.SIGALRM,previous)
            os.close(fd)

    def receipt(self, task_id):
        path = safe(self.reservation/'owner.json')
        owner = load(path)
        if owner is None: owner = load(safe(self.history/task_id/'owner.json'))
        if not owner or owner.get('id') != task_id: raise TaskError('task reservation cannot be confirmed')
        return owner

    def actor(self, owner, pid, role):
        mark = process_mark(pid)
        if not mark or mark != owner.get(role): raise TaskError('task actor mismatch')

    def retire(self, owner):
        if self.reservation.exists():
            self.history.mkdir(mode=0o700,exist_ok=True)
            destination = safe(self.history/owner['id'])
            if destination.exists(): raise TaskError('reservation receipt already exists')
            os.rename(str(self.reservation),str(destination))

    def save(self, value):
        publish(self.job(value['id'])/'state.json',value)
        publish(self.latest,value)

    def apply(self, operation, task_id, pid, payload):
        with self.lock():
            if operation == 'reserve':
                if load(self.publication) is not None: raise TaskError('previous publication is unconfirmed')
                value = self.validate(payload)
                if value['id'] != task_id or value['status'] != 'pending': raise TaskError('invalid initial task')
                previous = load(self.latest)
                if previous is not None:
                    self.validate(previous)
                    if previous['status'] in ACTIVE: raise TaskError('existing task requires confirmation')
                    owner = self.receipt(previous['id'])
                    if owner.get('phase') != 'ended' or (owner.get('runner') and alive(owner['runner'])): raise TaskError('previous runner is not confirmed stopped')
                if self.reservation.exists(): raise TaskError('another task owns the reservation')
                creator = process_mark(pid)
                if not creator: raise TaskError('creator process is unavailable')
                self.reservation.mkdir(mode=0o700)
                owner = dict(id=task_id,phase='launching',creator=creator)
                publish(self.reservation/'owner.json',owner)
                directory = self.job(task_id)
                safe(directory.parent).mkdir(mode=0o700,parents=True,exist_ok=True)
                directory.mkdir(mode=0o700)
                self.save(value)
                return value
            value = load(self.latest)
            if value is None or self.validate(value)['id'] != task_id: raise TaskError('current task ID changed')
            owner = self.receipt(task_id)
            if operation == 'launch_unknown':
                if owner.get('phase') == 'launching':
                    self.actor(owner,pid,'creator')
                    owner['phase'] = 'launch_unknown'
                    publish(self.reservation/'owner.json',owner)
                return value
            if operation == 'claim':
                if owner.get('phase') not in ('launching','launch_unknown'): raise TaskError('runner already claimed or task ended')
                runner = process_mark(pid)
                if not runner: raise TaskError('runner process is unavailable')
                owner.update(phase='runner',runner=runner)
                publish(self.reservation/'owner.json',owner)
                payload = dict(status='running',stage='precheck',message='Runner accepted the task.',exit_code=0,error='')
            elif operation == 'abort':
                if owner.get('phase') != 'launching': raise TaskError('task may already have started')
                self.actor(owner,pid,'creator')
            elif operation == 'clear':
                if payload.get('unit_stopped') is not True: raise TaskError('runner unit is not confirmed stopped')
                if owner.get('phase') == 'launch_unknown': raise TaskError('launch outcome needs independent review')
                if owner.get('phase') == 'launching' and alive(owner['creator']): raise TaskError('creator may still launch the task')
                if owner.get('runner') and alive(owner['runner']): raise TaskError('runner process is still active')
                if value['status'] in ACTIVE:
                    updated = datetime.datetime.strptime(value['updated_at'],'%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=datetime.timezone.utc)
                    if (datetime.datetime.now(datetime.timezone.utc)-updated).total_seconds() <= 600: raise TaskError('task is not stale')
            else:
                if operation not in ('progress','finish') or owner.get('phase') != 'runner': raise TaskError('invalid task operation')
                self.actor(owner,pid,'runner')
            barrier = load(self.publication)
            if barrier is not None and barrier.get('id') != task_id: raise TaskError('publication belongs to another task')
            if value['status'] not in ACTIVE and not (operation == 'clear' and barrier is not None): raise TaskError('terminal task cannot be overwritten')
            target = payload.get('status')
            code = payload.get('exit_code',0)
            if not isinstance(code,int) or not 0 <= code <= 255: raise TaskError('invalid task exit code')
            if operation == 'progress' or operation == 'claim':
                if target not in ACTIVE or ACTIVE[target] < ACTIVE[value['status']]: raise TaskError('task progress cannot go backwards')
            elif target not in ('success','failed') or (target == 'success' and (value['status'] != 'restarting' or code != 0)) or (target == 'failed' and code == 0):
                raise TaskError('invalid terminal result')
            terminal = operation in ('finish','abort','clear')
            if terminal:
                if barrier is None: publish(self.publication,dict(id=task_id))
                owner['phase'] = 'ended'
                if self.reservation.exists(): publish(self.reservation/'owner.json',owner)
                self.retire(owner)
            result = dict(value)
            for key in ('status','stage','message','exit_code','error'):
                if key in payload: result[key] = payload[key]
            result['updated_at'] = datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
            if target in ('success','failed'): result['completed_at'] = result['updated_at']
            result.pop('stale',None)
            result.pop('stale_reason',None)
            self.save(result)
            if terminal:
                confirmed = safe(self.history/task_id/'publication-confirmed.json')
                if confirmed.exists(): raise TaskError('publication receipt already exists')
                os.rename(str(self.publication),str(confirmed))
            return result

def main():
    try:
        root, work_root, operation, task_id, pid = sys.argv[1:]
        raw = sys.stdin.buffer.read(1048577)
        if len(raw)>1048576: raise TaskError('task request too large')
        payload = json.loads(raw.decode('utf-8'))
        if not isinstance(payload,dict): raise TaskError('invalid task request')
        result = TaskState(root,work_root).apply(operation,task_id,pid,payload)
        print(json.dumps(dict(ok=True,task=result),ensure_ascii=False))
    except (OSError,ValueError,KeyError,TypeError,IndexError,TaskError):
        print(json.dumps(dict(ok=False,error='Task state operation failed or is unconfirmed; reservation and evidence must be retained.')))
        raise SystemExit(1)

if __name__ == '__main__': main()
`

type upgradeCommand func(context.Context, string, []string, []byte) ([]byte, error)

type upgradeManager struct {
	root     string
	workRoot string
	cli      string
	command  upgradeCommand
}

func newUpgradeManager() upgradeManager {
	return upgradeManager{root: installDir, workRoot: "/root/codew-upgrades", cli: "/usr/local/bin/codew", command: runUpgradeCommand}
}

func runUpgradeCommand(ctx context.Context, name string, args []string, input []byte) ([]byte, error) {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(bounded, name, args...)
	cmd.Stdin = bytes.NewReader(input)
	return cmd.CombinedOutput()
}

func validUpgradeTaskID(id string) bool {
	return regexp.MustCompile(`^upgrade_[0-9]{8}_[0-9]{6}_[0-9a-f]{16}$`).MatchString(id)
}

func newUpgradeTaskID() (string, error) {
	var suffix [8]byte
	if _, err := io.ReadFull(rand.Reader, suffix[:]); err != nil {
		return "", fmt.Errorf("cannot create unique task ID")
	}
	return "upgrade_" + time.Now().UTC().Format("20060102_150405") + "_" + hex.EncodeToString(suffix[:]), nil
}

func (m upgradeManager) jobDir(id string) string {
	return filepath.Join(m.root, "logs", "upgrades", id)
}
func (m upgradeManager) statePath() string {
	return filepath.Join(m.root, "data", "upgrade-task-latest.json")
}

func safeUpgradePath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("unsafe upgrade path")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("upgrade path contains a link")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("upgrade path is unavailable")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}

func (m upgradeManager) validateTask(s upgradeTaskState) error {
	if !validUpgradeTaskID(s.ID) || (!isActiveUpgradeStatus(s.Status) && s.Status != "success" && s.Status != "failed") {
		return fmt.Errorf("invalid upgrade task identity or status")
	}
	if err := validateUpdateManifest(s.Manifest); err != nil {
		return fmt.Errorf("invalid task manifest")
	}
	if s.Package != s.Manifest.Package || s.DownloadURL != s.Manifest.DownloadURL || s.SHA256 != s.Manifest.SHA256 || s.TargetVersion != s.Manifest.Latest || s.CurrentVersion == "" {
		return fmt.Errorf("task manifest does not match task state")
	}
	if s.LogFile != filepath.Join(m.jobDir(s.ID), "runner.log") || s.WorkDir != filepath.Join(m.workRoot, s.ID) {
		return fmt.Errorf("task paths do not match this installation")
	}
	return nil
}

func (m upgradeManager) read() (upgradeTaskState, error) {
	var s upgradeTaskState
	if err := safeUpgradePath(m.statePath()); err != nil {
		return s, err
	}
	f, err := os.Open(m.statePath())
	if err != nil {
		return s, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return s, fmt.Errorf("upgrade state is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 1048577))
	if err != nil || len(data) > 1048576 {
		return s, fmt.Errorf("upgrade state is unreadable or too large")
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("upgrade state JSON is invalid")
	}
	if err := m.validateTask(s); err != nil {
		return s, err
	}
	return s, nil
}

func (m upgradeManager) operation(ctx context.Context, op string, taskID string, payload any) (upgradeTaskState, error) {
	var reply struct {
		OK   bool             `json:"ok"`
		Task upgradeTaskState `json:"task"`
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return reply.Task, fmt.Errorf("invalid task operation")
	}
	out, runErr := m.command(ctx, "python3", []string{"-c", upgradeStateProgram, m.root, m.workRoot, op, taskID, fmt.Sprint(os.Getpid())}, data)
	if runErr != nil || json.Unmarshal(out, &reply) != nil || !reply.OK || m.validateTask(reply.Task) != nil || reply.Task.ID != taskID {
		return upgradeTaskState{}, fmt.Errorf("任务状态操作失败或结果未确认；请保留任务预留和证据，不要自动重试")
	}
	return reply.Task, nil
}

func (r *Router) updateTaskLatest(w http.ResponseWriter, req *http.Request) {
	newUpgradeManager().latest(w, req)
}

func (r *Router) updateTaskLogs(w http.ResponseWriter, req *http.Request) {
	newUpgradeManager().logs(w, req)
}

func (r *Router) updateTaskPrecheck(w http.ResponseWriter, req *http.Request) {
	newUpgradeManager().precheck(w, req)
}

func (r *Router) updateTaskClearStale(w http.ResponseWriter, req *http.Request) {
	newUpgradeManager().clearStale(w, req)
}

func (r *Router) updateTaskCreate(w http.ResponseWriter, req *http.Request) {
	newUpgradeManager().create(w, req)
}

func isActiveUpgradeStatus(status string) bool {
	switch status {
	case "pending", "running", "queued", "downloading", "verifying", "backing_up", "installing", "restarting":
		return true
	default:
		return false
	}
}

func normalizeUpgradeTaskState(s upgradeTaskState) upgradeTaskState {
	if !isActiveUpgradeStatus(s.Status) {
		return s
	}
	updated, err := time.Parse(time.RFC3339, s.UpdatedAt)
	if err != nil || time.Since(updated) > 10*time.Minute {
		s.Stale = true
		s.StaleReason = "任务状态超时或时间无效；须确认 runner 和任务预留，不能仅凭当前版本判定成功。"
	}
	return s
}

func (m upgradeManager) hasEvidence() bool {
	for _, path := range []string{
		filepath.Join(m.root, "data", ".upgrade-reservation"),
		filepath.Join(m.root, "data", ".upgrade-publication"),
		filepath.Join(m.root, "data", ".upgrade-state.lock"),
		filepath.Join(m.root, "logs", "upgrades"),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

func (m upgradeManager) publicationUnconfirmed() bool {
	_, err := os.Lstat(filepath.Join(m.root, "data", ".upgrade-publication"))
	return !errors.Is(err, os.ErrNotExist)
}

func (m upgradeManager) latest(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	state, err := m.read()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !m.hasEvidence() {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "has_task": false, "message": "暂无升级任务"})
		} else {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "has_task": false, "message": "升级任务状态不可确认；未修改状态或释放任务预留。"})
		}
		return
	}
	if m.publicationUnconfirmed() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "has_task": false, "message": "任务结果发布尚未确认；保留预留和证据，不宣称升级成功。"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "has_task": true, "task": normalizeUpgradeTaskState(state)})
}

func (m upgradeManager) logs(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	state, err := m.read()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "无法确认日志所属任务；未读取其它文件。"})
		return
	}
	if err := safeUpgradePath(state.LogFile); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "任务日志路径不可信。"})
		return
	}
	file, err := os.Open(state.LogFile)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "任务日志暂不可读取。"})
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "任务日志不是普通文件。"})
		return
	}
	if info.Size() > 128*1024 {
		if _, err := file.Seek(info.Size()-128*1024, io.SeekStart); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "任务日志读取失败。"})
			return
		}
	}
	text, err := io.ReadAll(io.LimitReader(file, 128*1024))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "任务日志读取失败。"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "log_file": state.LogFile, "logs": string(text)})
}

type upgradeRuntime struct {
	AppDir    string `json:"app_dir"`
	ConfigDir string `json:"config_dir"`
	Mode      string `json:"mode"`
	Version   string `json:"version"`
	PID       string `json:"pid"`
	APIPort   string `json:"api_port"`
	PanelPort string `json:"panel_port"`
	WebPath   string `json:"web_path"`
}

func (m upgradeManager) probeScript(pid string) string {
	return strings.Join([]string{
		"set -euo pipefail",
		"[[ -f " + shellEscape(m.cli) + " && ! -L " + shellEscape(m.cli) + " ]]",
		"unset CONFIG_DIR",
		"source " + shellEscape(m.cli),
		"declare -F validate_upgrade_archive >/dev/null",
		"declare -F create_backup >/dev/null",
		"legacy_namespace_preflight",
		"load_operational_config",
		"[[ \"$MODE\" == fast && \"$APP_DIR\" == " + shellEscape(m.root) + " && \"$EXPECTED_VERSION\" == " + shellEscape(panelVersion) + " ]]",
		"restore_runtime_preflight",
		"[[ \"$(systemctl show codew-api -p MainPID --value)\" == " + shellEscape(pid) + " ]]",
		"optional_service_check codew-agent \"$AUTO_AGENT\" >/dev/null",
		"requested=false; if [[ \"$AUTO_AGENT\" == true && \"$SETUP_XRAY_SERVICE\" == true ]]; then requested=true; fi",
		"optional_service_check xray \"$requested\" >/dev/null",
		"for tool in systemd-run systemctl python3 curl unzip sha256sum; do command -v \"$tool\" >/dev/null; done",
	}, "\n")
}

func (m upgradeManager) runtimeContext(ctx context.Context) (upgradeRuntime, error) {
	var result upgradeRuntime
	script := m.probeScript(strconv.Itoa(os.Getpid())) + `
python3 - "$APP_DIR" "$CONFIG_DIR" "$MODE" "$EXPECTED_VERSION" "$API_PORT" "$PORT" "$WEB_BASE_PATH" "$(systemctl show codew-api -p MainPID --value)" <<'PY_RUNTIME'
import json,sys
keys=('app_dir','config_dir','mode','version','api_port','panel_port','web_path','pid')
print(json.dumps(dict(zip(keys,sys.argv[1:]))))
PY_RUNTIME
`
	out, err := m.command(ctx, "bash", []string{"-c", script}, nil)
	if err != nil || json.Unmarshal(out, &result) != nil || result.AppDir != m.root || result.Mode != "fast" || result.Version != panelVersion || result.PID != strconv.Itoa(os.Getpid()) {
		return upgradeRuntime{}, fmt.Errorf("只支持已确认归属的标准目录 fast/systemd 安装；请使用人工命令核查其它安装")
	}
	if safeUpgradePath(result.ConfigDir) != nil || !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(result.WebPath) {
		return upgradeRuntime{}, fmt.Errorf("安装配置映射无法确认")
	}
	for _, value := range []string{result.APIPort, result.PanelPort} {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return upgradeRuntime{}, fmt.Errorf("安装端口无法确认")
		}
	}
	return result, nil
}

func (m upgradeManager) diskCheck(ctx context.Context) bool {
	out, err := m.command(ctx, "df", []string{"-Pk", filepath.Dir(m.workRoot)}, nil)
	if err != nil {
		return false
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return false
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 6 {
		return false
	}
	kb, err := strconv.ParseUint(fields[len(fields)-3], 10, 64)
	return err == nil && kb >= 1024*1024
}

func (m upgradeManager) precheck(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	_, runtimeErr := m.runtimeContext(req.Context())
	s, stateErr := m.read()
	available := errors.Is(stateErr, os.ErrNotExist) && !m.hasEvidence()
	if stateErr == nil {
		available = !isActiveUpgradeStatus(s.Status)
		for _, name := range []string{".upgrade-reservation", ".upgrade-publication"} {
			if _, err := os.Lstat(filepath.Join(m.root, "data", name)); !errors.Is(err, os.ErrNotExist) {
				available = false
			}
		}
	}
	_, manifestErr := validUpdateURL(updateManifestURL())
	checks := []map[string]any{
		{"name": "运行模式与归属", "ok": runtimeErr == nil, "message": "仅支持已确认的标准目录 fast/systemd，要求 systemd-run；不使用 setsid 降级启动"},
		{"name": "远程版本清单", "ok": manifestErr == nil, "message": "使用 CODEW_UPDATE_MANIFEST_URL；下载前仍须严格校验清单"},
		{"name": "磁盘空间", "ok": m.diskCheck(req.Context()), "message": "下载工作目录所在文件系统至少需要 1GB 可用空间"},
		{"name": "任务预留", "ok": available, "message": "此处只读观察；创建时由跨进程状态事务独占预留"},
	}
	ok := true
	for _, item := range checks {
		if item["ok"] != true {
			ok = false
		}
	}
	message := "预检未通过；未创建或更改任务。"
	if ok {
		message = "只读预检通过；创建时将重新核验并申请任务预留。"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "checks": checks, "message": message})
}

func upgradeTaskUnit(id string) string {
	return "codew-upgrade-" + strings.ReplaceAll(id, "_", "-") + ".service"
}

func (m upgradeManager) unitStopped(ctx context.Context, task upgradeTaskState) bool {
	properties := "LoadState,ActiveState,SubState,MainPID,ControlPID,Job,ExecStart,Transient,KillMode,DropInPaths"
	out, err := m.command(ctx, "systemctl", []string{"show", upgradeTaskUnit(task.ID), "--property=" + properties, "--no-pager"}, nil)
	if err != nil {
		return false
	}
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return false
		}
		if _, exists := values[key]; exists {
			return false
		}
		values[key] = value
	}
	if values["MainPID"] != "0" || values["ControlPID"] != "0" || values["Job"] != "0" {
		return false
	}
	if values["ActiveState"] != "inactive" && values["ActiveState"] != "failed" {
		return false
	}
	if values["SubState"] != "dead" && values["SubState"] != "failed" {
		return false
	}
	if values["LoadState"] == "not-found" {
		return true
	}
	if values["LoadState"] != "loaded" || values["Transient"] != "yes" || values["KillMode"] != "control-group" || values["DropInPaths"] != "" {
		return false
	}
	prefix := "{ path=/bin/bash ; argv[]=/bin/bash " + filepath.Join(m.jobDir(task.ID), "runner.sh") + " ;"
	return strings.HasPrefix(values["ExecStart"], prefix) && strings.Count(values["ExecStart"], "path=") == 1
}

func (m upgradeManager) clearStale(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	s, err := m.read()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "无法确认任务，未清理任何预留或证据。"})
		return
	}
	s = normalizeUpgradeTaskState(s)
	barrier := false
	for _, name := range []string{".upgrade-reservation", ".upgrade-publication"} {
		if _, err := os.Lstat(filepath.Join(m.root, "data", name)); !errors.Is(err, os.ErrNotExist) {
			barrier = true
		}
	}
	if !isActiveUpgradeStatus(s.Status) && !barrier {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "任务已结束，日志和备份保持不变。", "task": s})
		return
	}
	if isActiveUpgradeStatus(s.Status) && !s.Stale {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "任务尚未达到陈旧条件；未清理。", "task": s})
		return
	}
	if !m.unitStopped(req.Context(), s) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "runner 仍活动、归属不符或状态未知；未清理。", "task": s})
		return
	}
	payload := map[string]any{"unit_stopped": true, "status": "failed", "stage": "stale_cleaned", "message": "管理员确认 runner 已结束；保留所有日志、备份和预留收据。", "exit_code": 98, "error": "task ended without a confirmed complete result"}
	confirmed, err := m.operation(req.Context(), "clear", s.ID, payload)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error(), "task": s})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": confirmed.Message, "task": confirmed})
}

func (m upgradeManager) create(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	runtime, err := m.runtimeContext(req.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if !m.diskCheck(req.Context()) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "磁盘空间不足或无法确认；未创建任务。"})
		return
	}
	manifestURL := updateManifestURL()
	manifest, err := fetchUpdateManifest(req.Context(), manifestURL)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "清单未通过校验，未创建任务：" + err.Error()})
		return
	}
	if !isRemoteVersionNewer(manifest.Latest, panelVersion) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "current_version": panelVersion, "target_version": manifest.Latest, "message": "同版或降级不创建任务。"})
		return
	}
	id, err := newUpgradeTaskID()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	task := upgradeTaskState{ID: id, Status: "pending", Stage: "created", Message: "任务已预留，尚未确认 runner 接管。",
		CurrentVersion: panelVersion, TargetVersion: manifest.Latest, Package: manifest.Package, DownloadURL: manifest.DownloadURL,
		SHA256: manifest.SHA256, Manifest: manifest, LogFile: filepath.Join(m.jobDir(id), "runner.log"),
		WorkDir: filepath.Join(m.workRoot, id), StartedAt: now, UpdatedAt: now}
	if _, err := m.operation(req.Context(), "reserve", id, task); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	script, err := m.writeRunner(task, runtime, manifestURL)
	if err != nil {
		result, saveErr := m.operation(context.Background(), "abort", id, map[string]any{"status": "failed", "stage": "prepare_failed", "message": "执行脚本准备失败，尚未调用启动命令。", "exit_code": 1, "error": "runner preparation failed"})
		message := "脚本准备失败；保留全部证据，未启动 runner。"
		if saveErr != nil {
			message = "脚本准备失败且状态未确认；不要自动重试。"
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": message, "task": result})
		return
	}
	_, startErr := m.command(req.Context(), "systemd-run", []string{"--unit", upgradeTaskUnit(id), "--property=Type=exec", "--property=KillMode=control-group", "/bin/bash", script}, nil)
	if startErr != nil {
		observed, markErr := m.operation(context.Background(), "launch_unknown", id, map[string]any{})
		message := "runner 启动结果未确认；保留任务预留，请勿再次启动。"
		if markErr != nil {
			message += "状态记录也未确认，须独立核查。"
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": message, "task": observed})
		return
	}
	observed, err := m.read()
	if err != nil || observed.ID != id || m.publicationUnconfirmed() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "启动命令已返回，但无法确认当前任务状态；不要自动重试。"})
		return
	}
	// Only the runner publishes progress after handoff.
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "任务已交接，是否升级成功以 runner 的完整终态为准。", "task": normalizeUpgradeTaskState(observed)})
}

func writePrivateUpgradeFile(path string, data []byte, mode os.FileMode) error {
	if err := safeUpgradePath(path); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func (m upgradeManager) writeRunner(task upgradeTaskState, runtime upgradeRuntime, manifestURL string) (string, error) {
	if m.validateTask(task) != nil || runtime.AppDir != m.root {
		return "", fmt.Errorf("invalid runner binding")
	}
	content, err := m.renderRunner(task, runtime, manifestURL)
	if err != nil {
		return "", err
	}
	path := filepath.Join(m.jobDir(task.ID), "runner.sh")
	if err := writePrivateUpgradeFile(task.LogFile, nil, 0600); err != nil {
		return "", err
	}
	if err := writePrivateUpgradeFile(path, []byte(content), 0700); err != nil {
		return "", err
	}
	return path, nil
}

func (m upgradeManager) renderRunner(task upgradeTaskState, runtime upgradeRuntime, manifestURL string) (string, error) {
	var renderErr error
	jsonPayload := func(value any) string {
		data, err := json.Marshal(value)
		if err != nil {
			renderErr = err
			return ""
		}
		return shellEscape(string(data))
	}
	progress := func(status, stage, message string) string {
		return "task_state progress " + jsonPayload(map[string]any{"status": status, "stage": stage, "message": message, "exit_code": 0, "error": ""})
	}
	header := []string{
		"#!/usr/bin/env bash", "set -euo pipefail", "umask 077",
		"TASK_ID=" + shellEscape(task.ID), "INSTALL_DIR=" + shellEscape(m.root),
		"WORK_ROOT=" + shellEscape(m.workRoot), "WORK_DIR=" + shellEscape(task.WorkDir),
		"LOG_FILE=" + shellEscape(task.LogFile), "PKG=" + shellEscape(task.Package),
		"DOWNLOAD_URL=" + shellEscape(task.DownloadURL), "SHA256=" + shellEscape(task.SHA256),
		"TARGET_VERSION=" + shellEscape(task.TargetVersion), "MANIFEST_URL=" + shellEscape(manifestURL),
		"STATE_PROGRAM=" + shellEscape(upgradeStateProgram), "CLAIMED=0",
	}
	start := `
task_state() {
  local operation="$1" payload="$2"
  printf '%s' "$payload" | python3 -c "$STATE_PROGRAM" "$INSTALL_DIR" "$WORK_ROOT" "$operation" "$TASK_ID" "$$" >/dev/null
}
fail() {
  local code="${1:-1}" stage="${2:-unexpected}" payload
  trap - ERR INT TERM HUP
  [[ "$code" =~ ^[0-9]+$ ]] && (( code >= 1 && code <= 255 )) || code=1
  printf 'Task failed at %s (exit=%s); keep all evidence.\n' "$stage" "$code" >&2
  if [[ "$CLAIMED" == 1 ]]; then
    if payload=$(python3 -c 'import json,sys; print(json.dumps(dict(status="failed",stage=sys.argv[2],message="Runner stopped before complete verification.",error="Runner stage failed.",exit_code=int(sys.argv[1]))))' "$code" "$stage"); then
      if ! task_state finish "$payload"; then printf 'Failure state is unconfirmed; retain reservation or publication receipt.\n' >&2; fi
    else
      printf 'Failure metadata is unconfirmed; retain reservation.\n' >&2
    fi
  fi
  exit "$code"
}
trap 'code=$?; fail "$code" unexpected' ERR
trap 'fail 130 interrupted' INT
trap 'fail 143 interrupted' TERM HUP
[[ -f "$LOG_FILE" && ! -L "$LOG_FILE" ]] || exit 1
exec >>"$LOG_FILE" 2>&1
task_state claim '{}'
CLAIMED=1
`
	steps := []string{
		m.probeScript(runtime.PID),
		"[[ \"$CONFIG_DIR\" == " + shellEscape(runtime.ConfigDir) + " ]]",
		"mkdir -p \"$WORK_ROOT\"",
		"[[ ! -e \"$WORK_DIR\" && ! -L \"$WORK_DIR\" ]]",
		"mkdir -m 700 \"$WORK_DIR\"",
		"cd \"$WORK_DIR\"",
		progress("downloading", "downloading", "Downloading the verified codew package."),
		"curl --fail --location --silent --show-error --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 300 -o \"$PKG\" \"$DOWNLOAD_URL\"",
		progress("verifying", "verifying", "Checking package SHA256 and ZIP members."),
		"printf '%s  %s\\n' \"$SHA256\" \"$PKG\" | sha256sum -c -",
		"validate_upgrade_archive \"$PKG\" " + shellEscape(strings.TrimSuffix(task.Package, ".zip")) + " || fail \"$?\" verifying",
		progress("backing_up", "backing_up", "Creating the existing CLI configuration/data backup."),
		"SAFETY_BACKUP=$(create_backup pre-upgrade fast)",
		"[[ -n \"$SAFETY_BACKUP\" && -f \"$SAFETY_BACKUP\" && ! -L \"$SAFETY_BACKUP\" ]]",
		"printf 'Private pre-upgrade backup retained.\\n'",
		"unzip -q \"$PKG\"",
		"cd " + shellEscape(strings.TrimSuffix(task.Package, ".zip")),
		progress("installing", "installing", "Running the verified installer."),
		"APP_DIR=\"$APP_DIR\" CONFIG_DIR=\"$CONFIG_DIR\" CODEW_INSTALL_MODE=fast FRESH_INSTALL=false CODEW_UPDATE_MANIFEST_URL=\"$MANIFEST_URL\" bash deploy/install.sh",
		progress("restarting", "verifying_health", "Verifying installed configuration, services and all API routes."),
		"load_operational_config",
		"[[ \"$EXPECTED_VERSION\" == \"$TARGET_VERSION\" && \"$MODE\" == fast && \"$APP_DIR\" == \"$INSTALL_DIR\" && \"$CONFIG_DIR\" == " + shellEscape(runtime.ConfigDir) + " ]]",
		"nginx -t",
		"systemctl is-active --quiet codew-api",
		"systemctl is-active --quiet nginx",
		"optional_service_check codew-agent \"$AUTO_AGENT\" >/dev/null",
		"requested=false; if [[ \"$AUTO_AGENT\" == true && \"$SETUP_XRAY_SERVICE\" == true ]]; then requested=true; fi",
		"optional_service_check xray \"$requested\" >/dev/null",
		"verified=false",
		"for attempt in {1..30}; do",
		"  if health_check \"http://127.0.0.1:$API_PORT/api/health\" \"$TARGET_VERSION\" && health_check \"http://127.0.0.1:$PORT/api/health\" \"$TARGET_VERSION\" && health_check \"http://127.0.0.1:$PORT/$WEB_BASE_PATH/api/health\" \"$TARGET_VERSION\"; then verified=true; break; fi",
		"  sleep 1",
		"done",
		"[[ \"$verified\" == true ]] || fail 34 verify",
		"task_state finish " + jsonPayload(map[string]any{"status": "success", "stage": "success", "message": "Installer, owned services and exact API health checks completed.", "exit_code": 0, "error": ""}),
		"trap - ERR INT TERM HUP",
		"exit 0",
	}
	if renderErr != nil {
		return "", fmt.Errorf("runner status JSON cannot be encoded")
	}
	return strings.Join(header, "\n") + "\n" + start + "\n" + strings.Join(steps, "\n") + "\n", nil
}
