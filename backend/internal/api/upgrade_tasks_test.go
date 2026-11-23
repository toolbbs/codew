package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func upgradeTestManager(t *testing.T) upgradeManager {
	t.Helper()
	return upgradeManager{root: t.TempDir(), workRoot: filepath.Join(t.TempDir(), "work"), cli: filepath.Join(t.TempDir(), "codew")}
}

func upgradeTestTask(m upgradeManager) upgradeTaskState {
	manifest := codewManifestFixture()
	id := "upgrade_20261117_120000_1111111111111111"
	now := time.Now().UTC().Format(time.RFC3339)
	return upgradeTaskState{ID: id, Status: "pending", Stage: "created", Message: "Synthetic pending task.",
		CurrentVersion: panelVersion, TargetVersion: manifest.Latest, Package: manifest.Package,
		DownloadURL: manifest.DownloadURL, SHA256: manifest.SHA256, Manifest: manifest,
		LogFile: filepath.Join(m.jobDir(id), "runner.log"), WorkDir: filepath.Join(m.workRoot, id),
		StartedAt: now, UpdatedAt: now}
}

func TestUpgradeTaskIDsDoNotCollideWithinSameSecond(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 500; i++ {
		id, err := newUpgradeTaskID()
		if err != nil || !validUpgradeTaskID(id) || seen[id] {
			t.Fatal("task ID is invalid, repeated or unavailable")
		}
		seen[id] = true
	}
}

func TestUpgradeStateReadsNeverCreateOrRewriteFiles(t *testing.T) {
	m := upgradeTestManager(t)
	if _, err := m.read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing state was not distinguished from a read failure")
	}
	if _, err := os.Stat(filepath.Join(m.root, "data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created installation directories")
	}
	if err := os.MkdirAll(filepath.Dir(m.statePath()), 0700); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"{broken", "null", "[]"} {
		if err := os.WriteFile(m.statePath(), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(m.statePath())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.read(); err == nil {
			t.Fatal("broken state was accepted")
		}
		after, err := os.Stat(m.statePath())
		if err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(m.statePath())
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != body || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
			t.Fatal("state read changed content, permissions or timestamp")
		}
	}
}

func TestUpgradeTaskPathsAndManifestMustMatch(t *testing.T) {
	m := upgradeTestManager(t)
	valid := upgradeTestTask(m)
	if err := m.validateTask(valid); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*upgradeTaskState){
		func(s *upgradeTaskState) { s.ID = "../foreign" },
		func(s *upgradeTaskState) { s.LogFile = filepath.Join(m.root, "foreign.log") },
		func(s *upgradeTaskState) { s.WorkDir = filepath.Join(m.workRoot, "foreign") },
		func(s *upgradeTaskState) { s.Package = "zxy-panel-v0.7.9-next.zip" },
		func(s *upgradeTaskState) { s.TargetVersion = "0.7.99-other" },
		func(s *upgradeTaskState) { s.SHA256 = strings.Repeat("b", 64) },
	} {
		value := valid
		change(&value)
		if err := m.validateTask(value); err == nil {
			t.Fatal("foreign task binding was accepted")
		}
	}
}

func TestUpgradeNormalizationIsPureAndNeverInfersSuccess(t *testing.T) {
	m := upgradeTestManager(t)
	value := upgradeTestTask(m)
	value.Status = "running"
	value.TargetVersion = panelVersion
	value.UpdatedAt = time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339)
	before := value
	got := normalizeUpgradeTaskState(value)
	if !reflect.DeepEqual(value, before) || got.Status != "running" || !got.Stale || got.CompletedAt != "" {
		t.Fatal("read normalization modified input or guessed success from the API version")
	}
	value.UpdatedAt = "invalid timestamp"
	if !normalizeUpgradeTaskState(value).Stale {
		t.Fatal("invalid timestamp was treated as a healthy running task")
	}
}

func TestUpgradeStateOperationChecksExternalCommitResult(t *testing.T) {
	m := upgradeTestManager(t)
	value := upgradeTestTask(m)
	calls := 0
	m.command = func(ctx context.Context, name string, args []string, input []byte) ([]byte, error) {
		calls++
		if name != "python3" || len(args) != 7 || args[0] != "-c" || args[1] != upgradeStateProgram || args[2] != m.root || args[3] != m.workRoot || args[4] != "reserve" || args[5] != value.ID {
			t.Fatal("task transaction crossed its declared boundary")
		}
		var decoded upgradeTaskState
		if err := json.Unmarshal(input, &decoded); err != nil || !reflect.DeepEqual(decoded, value) {
			t.Fatal("candidate task changed before submission")
		}
		return json.Marshal(map[string]any{"ok": true, "task": value})
	}
	got, err := m.operation(context.Background(), "reserve", value.ID, value)
	if err != nil || !reflect.DeepEqual(got, value) || calls != 1 {
		t.Fatal("confirmed state transaction did not return its actual result")
	}
	for _, reply := range []struct {
		body []byte
		err  error
	}{
		{[]byte(`{"ok":false}`), nil},
		{[]byte("invalid JSON"), nil},
		{[]byte(`{"ok":true}`), nil},
		{[]byte(`{"ok":true}`), errors.New("unconfirmed")},
	} {
		calls = 0
		m.command = func(context.Context, string, []string, []byte) ([]byte, error) { calls++; return reply.body, reply.err }
		got, err = m.operation(context.Background(), "reserve", value.ID, value)
		if err == nil || got.ID != "" || calls != 1 {
			t.Fatal("failed or unconfirmed state operation was retried or reported successful")
		}
	}
}

func TestUpgradeStateProgramFixture(t *testing.T) {
	if !strings.Contains(upgradeStateProgram, "fcntl.flock") || !strings.Contains(upgradeStateProgram, "os.replace") {
		t.Fatal("shared state transaction is missing")
	}
	if directory := os.Getenv("CODEW_UPGRADE_TEST_EVIDENCE"); directory != "" {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		m := upgradeManager{root: "/opt/codew", workRoot: "/root/codew-upgrades", cli: "/usr/local/bin/codew"}
		info := upgradeRuntime{AppDir: m.root, ConfigDir: "/etc/codew", Mode: "fast", Version: panelVersion, PID: "777", APIPort: "18088", PanelPort: "49321", WebPath: "fixturebase"}
		manifest := codewManifestFixture()
		if input := os.Getenv("CODEW_UPGRADE_RUNNER_INPUT"); input != "" {
			fixture, err := readUpgradeRunnerFixture(input)
			if err != nil {
				t.Fatal(err)
			}
			m.root, m.workRoot, m.cli = fixture.App, fixture.Work, fixture.CLI
			info.AppDir, info.ConfigDir = fixture.App, fixture.Config
			archive, err := os.ReadFile(fixture.Archive)
			if err != nil {
				t.Fatal(err)
			}
			manifest.SHA256 = fmt.Sprintf("%x", sha256.Sum256(archive))
		}
		task := upgradeTestTask(m)
		task.Manifest, task.SHA256 = manifest, manifest.SHA256
		runner, err := m.renderRunner(task, info, "https://manifest.invalid/version.json?channel=test&v=1")
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(map[string]any{"source_os": runtime.GOOS, "state_program": upgradeStateProgram, "manifest": manifest, "task": task, "runner": runner, "runtime": info, "cli": m.cli})
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(filepath.Join(directory, "state-fixture.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

type upgradeRunnerFixtureInput struct {
	Sandbox string `json:"sandbox"`
	App     string `json:"app"`
	Config  string `json:"config"`
	Work    string `json:"work"`
	CLI     string `json:"cli"`
	Archive string `json:"archive"`
}

func readUpgradeRunnerFixture(path string) (upgradeRunnerFixtureInput, error) {
	var value upgradeRunnerFixtureInput
	invalid := errors.New("runner execution requires an existing private temporary sandbox and regular synthetic inputs")
	reject := func(check string, index int, candidate, resolved string, cause error) error {
		return fmt.Errorf("%w [check=%s index=%d candidate=%q resolved=%q os_temp=%q cause=%v]", invalid, check, index, candidate, resolved, os.TempDir(), cause)
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 16384 {
		return value, reject("input_read_or_size", -1, path, "", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&value) != nil || decoder.Decode(&extra) != io.EOF {
		return value, reject("input_json", -1, path, "", nil)
	}
	inside := func(root, child string) bool {
		relative, err := filepath.Rel(root, child)
		return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
	}
	if !filepath.IsAbs(value.Sandbox) || !inside(os.TempDir(), value.Sandbox) {
		return value, reject("sandbox_scope", 0, value.Sandbox, "", nil)
	}
	for index, candidate := range []string{value.Sandbox, value.App, value.Config, value.Work, value.CLI, value.Archive, path} {
		if !filepath.IsAbs(candidate) || filepath.Clean(candidate) != candidate || strings.ContainsAny(candidate, "\x00\r\n") || (index != 0 && !inside(value.Sandbox, candidate)) {
			return value, reject("candidate_path", index, candidate, "", nil)
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil || resolved != candidate {
			return value, reject("path_resolution", index, candidate, resolved, err)
		}
		info, err := os.Lstat(candidate)
		if err != nil || (index < 4 && !info.IsDir()) || (index >= 4 && !info.Mode().IsRegular()) {
			return value, reject("file_kind", index, candidate, "", err)
		}
		if index == 0 && runtime.GOOS == "linux" && info.Mode().Perm()&0077 != 0 {
			return value, reject("sandbox_permissions", index, candidate, "", nil)
		}
		if index == 5 && info.Size() > 8*1024*1024 {
			return value, reject("archive_size", index, candidate, "", nil)
		}
	}
	if value.App == value.Config || value.App == value.Work || value.Config == value.Work ||
		inside(value.App, value.Config) || inside(value.Config, value.App) || inside(value.App, value.Work) || inside(value.Work, value.App) {
		return value, reject("role_overlap", -1, value.Sandbox, "", nil)
	}
	return value, nil
}

func retainUpgradeRunnerFixtureFailure(t *testing.T, root string, rejection error) {
	t.Helper()
	directory := os.Getenv("CODEW_UPGRADE_TEST_EVIDENCE")
	if directory == "" {
		t.Log("Failed fixture retention requires CODEW_UPGRADE_TEST_EVIDENCE; original failure is unchanged")
		return
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Log("Failed fixture evidence directory is unavailable:", err)
		return
	}
	destination, err := os.MkdirTemp(directory, "rejected-sandbox-")
	if err != nil {
		t.Log("Failed fixture destination cannot be created:", err)
		return
	}
	writeNew := func(path string, data []byte) error {
		output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := output.Write(data)
		closeErr := output.Close()
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	var records []map[string]any
	err = filepath.WalkDir(root, func(path string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		record := map[string]any{"path": relative, "mode": info.Mode().String(), "size": info.Size(), "mtime_ns": info.ModTime().UnixNano()}
		records = append(records, record)
		target := filepath.Join(destination, "tree", relative)
		if info.IsDir() {
			return os.Mkdir(target, 0700)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			record["link"] = link
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported diagnostic fixture file type: %s", info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		record["sha256"] = fmt.Sprintf("%x", sha256.Sum256(data))
		return writeNew(target, data)
	})
	metadata := map[string]any{"source_root": root, "os_temp": os.TempDir(), "goos": runtime.GOOS, "go_version": runtime.Version(),
		"rejection": fmt.Sprint(rejection), "copy_error": fmt.Sprint(err), "records": records,
		"metadata_note": "Original modes and modification times are recorded; copies are private and symlinks are not followed."}
	data, encodeErr := json.MarshalIndent(metadata, "", "  ")
	if encodeErr != nil {
		t.Log("Failed fixture metadata cannot be encoded:", encodeErr)
		return
	}
	if writeErr := writeNew(filepath.Join(destination, "diagnostic.json"), data); writeErr != nil {
		t.Log("Failed fixture metadata cannot be retained:", writeErr)
		return
	}
	t.Log("Failed fixture evidence retained:", destination, "copy error:", err)
}

func TestUpgradeRunnerFixtureRejectsNonSandboxInputs(t *testing.T) {
	t.Setenv("GOTMPDIR", os.TempDir())
	root := t.TempDir()
	var rejection error
	t.Cleanup(func() {
		if t.Failed() {
			retainUpgradeRunnerFixtureFailure(t, root, rejection)
		}
	})
	value := upgradeRunnerFixtureInput{Sandbox: root, App: filepath.Join(root, "app"), Config: filepath.Join(root, "config"),
		Work: filepath.Join(root, "work"), CLI: filepath.Join(root, "cli"), Archive: filepath.Join(root, "synthetic.zip")}
	for _, directory := range []string{value.App, value.Config, value.Work} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{value.CLI, value.Archive} {
		if err := os.WriteFile(file, []byte("synthetic test input"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	input := filepath.Join(root, "runner-input.json")
	write := func(candidate upgradeRunnerFixtureInput) {
		data, err := json.Marshal(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(input, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(value)
	if got, err := readUpgradeRunnerFixture(input); err != nil || got != value {
		rejection = err
		t.Fatal("private synthetic fixture was rejected", err)
	}
	for _, mutate := range []func(*upgradeRunnerFixtureInput){
		func(v *upgradeRunnerFixtureInput) { v.Sandbox = os.TempDir() },
		func(v *upgradeRunnerFixtureInput) { v.App = filepath.Join(os.TempDir(), "foreign-app") },
		func(v *upgradeRunnerFixtureInput) { v.Config = v.App },
		func(v *upgradeRunnerFixtureInput) { v.Work = v.App },
		func(v *upgradeRunnerFixtureInput) { v.CLI = v.App },
		func(v *upgradeRunnerFixtureInput) { v.Archive = filepath.Join(root, "missing.zip") },
	} {
		candidate := value
		mutate(&candidate)
		write(candidate)
		if _, err := readUpgradeRunnerFixture(input); err == nil {
			t.Fatal("unsafe fixture input was accepted")
		}
	}
	write(value)
	for _, suffix := range []string{` {}`, ` trailing`} {
		data, err := os.ReadFile(input)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(input, append(data, []byte(suffix)...), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readUpgradeRunnerFixture(input); err == nil {
			t.Fatal("trailing fixture input was accepted")
		}
		write(value)
	}
}

type upgradeFileRecord struct {
	data     string
	mode     os.FileMode
	modified time.Time
}

func upgradeTree(t *testing.T, root string) map[string]upgradeFileRecord {
	t.Helper()
	result := make(map[string]upgradeFileRecord)
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		data := ""
		if info.Mode().IsRegular() {
			value, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			data = string(value)
		}
		result[path] = upgradeFileRecord{data, info.Mode(), info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertUpgradeTree(t *testing.T, root string, before map[string]upgradeFileRecord) {
	t.Helper()
	after := upgradeTree(t, root)
	if len(before) != len(after) {
		diagnoseUpgradeTree(t, root, before, after)
		t.Fatal("read-only operation changed the file set")
	}
	for name, value := range before {
		got, ok := after[name]
		if !ok || value.data != got.data || value.mode != got.mode || !value.modified.Equal(got.modified) {
			diagnoseUpgradeTree(t, root, before, after)
			t.Fatal("read-only operation changed bytes, permissions or timestamps")
		}
	}
}

func upgradeFileDiagnostic(value upgradeFileRecord, exists bool) map[string]any {
	if !exists {
		return nil
	}
	kind := "other"
	digest := ""
	if value.mode.IsDir() {
		kind = "directory"
	} else if value.mode.IsRegular() {
		kind = "regular"
		digest = fmt.Sprintf("%x", sha256.Sum256([]byte(value.data)))
	}
	return map[string]any{
		"kind": kind, "mode": value.mode.String(), "mode_bits": uint32(value.mode),
		"byte_count": len(value.data), "sha256": digest,
		"modified": value.modified.String(), "modified_utc": value.modified.UTC().Format(time.RFC3339Nano),
		"modified_unix_nano": value.modified.UnixNano(),
	}
}

func diagnoseUpgradeTree(t *testing.T, root string, before, after map[string]upgradeFileRecord) {
	t.Helper()
	paths := make(map[string]bool)
	for path := range before {
		paths[path] = true
	}
	for path := range after {
		paths[path] = true
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	snapshots := map[string]map[string]any{"before": {}, "after": {}}
	changes := make([]map[string]any, 0)
	for _, path := range ordered {
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			t.Logf("diagnostic path is outside the test root: %q, error=%v", path, err)
			return
		}
		b, hasBefore := before[path]
		a, hasAfter := after[path]
		name := filepath.ToSlash(relative)
		bInfo, aInfo := upgradeFileDiagnostic(b, hasBefore), upgradeFileDiagnostic(a, hasAfter)
		if hasBefore {
			snapshots["before"][name] = bInfo
		}
		if hasAfter {
			snapshots["after"][name] = aInfo
		}
		if hasBefore != hasAfter || b.data != a.data || b.mode != a.mode || !b.modified.Equal(a.modified) {
			change := map[string]any{"path": name, "before": bInfo, "after": aInfo,
				"same_bytes":    hasBefore && hasAfter && b.data == a.data,
				"same_mode":     hasBefore && hasAfter && b.mode == a.mode,
				"same_modified": hasBefore && hasAfter && b.modified.Equal(a.modified)}
			changes = append(changes, change)
			data, err := json.Marshal(change)
			if err != nil {
				t.Logf("cannot encode diagnostic: %v", err)
			} else {
				t.Logf("READONLY_DIFF %s", data)
			}
		}
	}
	base := os.Getenv("CODEW_UPGRADE_DIAGNOSTICS_DIR")
	relative, err := filepath.Rel(root, base)
	if !filepath.IsAbs(base) || err != nil || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		t.Log("external diagnostic directory is unavailable; original safety assertion will still fail")
		return
	}
	directory, err := os.MkdirTemp(base, "get-readonly-failure-")
	if err != nil {
		t.Logf("cannot reserve external diagnostic directory: %v", err)
		return
	}
	write := func(path string, data []byte) error {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(data)
		return errors.Join(writeErr, file.Close())
	}
	data, err := json.MarshalIndent(map[string]any{"test": t.Name(), "root": root,
		"goos": runtime.GOOS, "go_version": runtime.Version(), "changes": changes, "snapshots": snapshots}, "", "  ")
	if err != nil {
		t.Logf("cannot encode full diagnostic snapshot: %v", err)
		return
	}
	if err := write(filepath.Join(directory, "snapshot.json"), data); err != nil {
		t.Logf("cannot save diagnostic snapshot: %v", err)
		return
	}
	for phase, records := range map[string]map[string]upgradeFileRecord{"before": before, "after": after} {
		for path, record := range records {
			if !record.mode.IsRegular() {
				continue
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				t.Logf("cannot resolve %s diagnostic path: %v", phase, err)
				continue
			}
			if err := write(filepath.Join(directory, phase, "files", relative), []byte(record.data)); err != nil {
				t.Logf("cannot save %s diagnostic bytes for %q: %v", phase, relative, err)
			}
		}
	}
	t.Logf("READONLY_EVIDENCE %s", directory)
}

func writeUpgradeTestState(t *testing.T, m upgradeManager, task upgradeTaskState) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(m.statePath()), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.statePath(), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func upgradeResponse(t *testing.T, handler http.HandlerFunc, method string) map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(method, "/api/updates/tasks", strings.NewReader("{}")))
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected response status: %d", recorder.Code)
	}
	var value map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

type upgradeHostFake struct {
	mu    sync.Mutex
	calls []string
	task  upgradeTaskState
	mode  string
}

func installUpgradeHostFake(t *testing.T, m *upgradeManager, mode string) *upgradeHostFake {
	t.Helper()
	host := &upgradeHostFake{mode: mode}
	m.command = func(ctx context.Context, name string, args []string, input []byte) ([]byte, error) {
		host.mu.Lock()
		defer host.mu.Unlock()
		host.calls = append(host.calls, name)
		switch name {
		case "bash":
			return json.Marshal(upgradeRuntime{AppDir: m.root, ConfigDir: filepath.Join(m.root, "config"), Mode: "fast", Version: panelVersion, PID: strconv.Itoa(os.Getpid()), APIPort: "18088", PanelPort: "49321", WebPath: "fixturebase"})
		case "df":
			return []byte("Filesystem 1024-blocks Used Available Capacity Mounted-on\nsynthetic 4000000 1 3999999 1% /\n"), nil
		case "python3":
			if len(args) != 7 || args[1] != upgradeStateProgram {
				return nil, errors.New("unexpected transaction")
			}
			op := args[4]
			host.calls[len(host.calls)-1] += ":" + op
			if op == "reserve" {
				if host.mode == "reserve-failed" || host.task.ID != "" {
					return nil, errors.New("synthetic state operation failure")
				}
				if err := json.Unmarshal(input, &host.task); err != nil {
					return nil, err
				}
				if err := os.MkdirAll(m.jobDir(host.task.ID), 0700); err != nil {
					return nil, err
				}
				writeUpgradeTestState(t, *m, host.task)
				if host.mode == "prepare-failed" {
					if err := os.WriteFile(host.task.LogFile, []byte("preserved log"), 0600); err != nil {
						return nil, err
					}
				}
			} else if op == "abort" {
				host.task.Status = "failed"
				host.task.Stage = "prepare_failed"
				writeUpgradeTestState(t, *m, host.task)
			} else if op != "launch_unknown" {
				return nil, errors.New("unexpected operation")
			}
			return json.Marshal(map[string]any{"ok": true, "task": host.task})
		case "systemd-run":
			if len(args) != 6 || args[0] != "--unit" || args[2] != "--property=Type=exec" || args[3] != "--property=KillMode=control-group" || args[4] != "/bin/bash" {
				return nil, errors.New("runner launch arguments changed")
			}
			host.task.Status = "running"
			host.task.Stage = "precheck"
			if host.mode == "fast-finish" {
				host.task.Status = "success"
				host.task.Stage = "success"
			}
			writeUpgradeTestState(t, *m, host.task)
			if host.mode == "start-unknown" {
				return nil, errors.New("synthetic launch result unknown")
			}
			return []byte("synthetic unit accepted"), nil
		default:
			return nil, errors.New("unexpected host action")
		}
	}
	return host
}

func TestUpgradeGETsAreReadOnlyAndCannotRecoverSuccess(t *testing.T) {
	m := upgradeTestManager(t)
	installUpgradeHostFake(t, &m, "")
	before := upgradeTree(t, m.root)
	for _, handler := range []http.HandlerFunc{m.latest, m.logs, m.precheck} {
		upgradeResponse(t, handler, http.MethodGet)
	}
	assertUpgradeTree(t, m.root, before)
	s := upgradeTestTask(m)
	s.Status = "running"
	s.UpdatedAt = time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339)
	s.Manifest.Version = "0.7.8"
	s.Manifest.Latest = panelVersion
	s.Manifest.Codename = "stable-engineering"
	s.Manifest.Package = "codew-v0.7.8-stable-engineering.zip"
	s.Manifest.DownloadURL = "https://release.invalid/" + s.Manifest.Package
	s.Package = s.Manifest.Package
	s.DownloadURL = s.Manifest.DownloadURL
	s.TargetVersion = panelVersion
	writeUpgradeTestState(t, m, s)
	if err := os.MkdirAll(m.jobDir(s.ID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.LogFile, []byte("synthetic old log"), 0600); err != nil {
		t.Fatal(err)
	}
	before = upgradeTree(t, m.root)
	response := upgradeResponse(t, m.latest, http.MethodGet)
	state := response["task"].(map[string]any)
	if state["status"] != "running" || state["stale"] != true {
		t.Fatal("GET inferred success from matching API version")
	}
	upgradeResponse(t, m.logs, http.MethodGet)
	upgradeResponse(t, m.precheck, http.MethodGet)
	assertUpgradeTree(t, m.root, before)
}

func TestUpgradeGETDistinguishesCorruptAndUnconfirmedState(t *testing.T) {
	m := upgradeTestManager(t)
	if err := os.MkdirAll(filepath.Dir(m.statePath()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.statePath(), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	before := upgradeTree(t, m.root)
	response := upgradeResponse(t, m.latest, http.MethodGet)
	if response["ok"] != false {
		t.Fatal("corrupt state was treated as no task")
	}
	assertUpgradeTree(t, m.root, before)
	s := upgradeTestTask(m)
	s.Status = "success"
	s.Stage = "success"
	writeUpgradeTestState(t, m, s)
	if err := os.WriteFile(filepath.Join(m.root, "data", ".upgrade-publication"), []byte(`{"id":"synthetic"}`), 0600); err != nil {
		t.Fatal(err)
	}
	before = upgradeTree(t, m.root)
	response = upgradeResponse(t, m.latest, http.MethodGet)
	if response["ok"] != false || response["task"] != nil {
		t.Fatal("unconfirmed publication was displayed as completed success")
	}
	assertUpgradeTree(t, m.root, before)
}

func TestUpgradeCreateNeverOverwritesFastRunnerResult(t *testing.T) {
	m := upgradeTestManager(t)
	host := installUpgradeHostFake(t, &m, "fast-finish")
	body, err := json.Marshal(codewManifestFixture())
	if err != nil {
		t.Fatal(err)
	}
	useUpdateTestClient(t, string(body))
	t.Setenv("CODEW_UPDATE_MANIFEST_URL", "https://manifest.invalid/version.json")
	response := upgradeResponse(t, m.create, http.MethodPost)
	if response["ok"] != true {
		t.Fatalf("handoff failed: %v", response["message"])
	}
	state, err := m.read()
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "success" || state.Stage != "success" {
		t.Fatal("API overwrote the fast runner terminal state")
	}
	if strings.Join(host.calls, ",") != "bash,df,python3:reserve,systemd-run" {
		t.Fatal("unexpected extra launch or state write after handoff")
	}
}

func TestUpgradeLaunchUnconfirmedDoesNotRetryOrClaimFailure(t *testing.T) {
	m := upgradeTestManager(t)
	host := installUpgradeHostFake(t, &m, "start-unknown")
	body, err := json.Marshal(codewManifestFixture())
	if err != nil {
		t.Fatal(err)
	}
	useUpdateTestClient(t, string(body))
	t.Setenv("CODEW_UPDATE_MANIFEST_URL", "https://manifest.invalid/version.json")
	response := upgradeResponse(t, m.create, http.MethodPost)
	if response["ok"] != false {
		t.Fatal("unconfirmed launch was reported successful")
	}
	state, err := m.read()
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "running" {
		t.Fatal("API replaced a runner result with a guessed failed or queued state")
	}
	if strings.Join(host.calls, ",") != "bash,df,python3:reserve,systemd-run,python3:launch_unknown" {
		t.Fatal("launch was retried, downgraded to setsid or overwritten")
	}
}

func TestUpgradeReserveAndPrepareFailuresNeverLaunch(t *testing.T) {
	for _, mode := range []string{"reserve-failed", "prepare-failed"} {
		t.Run(mode, func(t *testing.T) {
			m := upgradeTestManager(t)
			host := installUpgradeHostFake(t, &m, mode)
			body, err := json.Marshal(codewManifestFixture())
			if err != nil {
				t.Fatal(err)
			}
			useUpdateTestClient(t, string(body))
			t.Setenv("CODEW_UPDATE_MANIFEST_URL", "https://manifest.invalid/version.json")
			response := upgradeResponse(t, m.create, http.MethodPost)
			if response["ok"] != false {
				t.Fatal("failed preparation was reported successful")
			}
			if strings.Contains(strings.Join(host.calls, ","), "systemd-run") {
				t.Fatal("failed reservation or preparation started a runner")
			}
			if mode == "prepare-failed" {
				content, err := os.ReadFile(host.task.LogFile)
				if err != nil {
					t.Fatal(err)
				}
				if string(content) != "preserved log" || host.task.Status != "failed" {
					t.Fatal("prepare failure overwrote old evidence or lost failure state")
				}
			}
		})
	}
}

func TestUpgradeStoppedUnitRequiresOwnershipAndNoProcesses(t *testing.T) {
	m := upgradeTestManager(t)
	s := upgradeTestTask(m)
	base := "LoadState=loaded\nActiveState=inactive\nSubState=dead\nMainPID=0\nControlPID=0\nJob=0\nTransient=yes\nKillMode=control-group\nDropInPaths=\nExecStart={ path=/bin/bash ; argv[]=/bin/bash " + filepath.Join(m.jobDir(s.ID), "runner.sh") + " ; }\n"
	for _, tc := range []struct {
		body    string
		allowed bool
	}{
		{base, true},
		{strings.ReplaceAll(base, "ActiveState=inactive", "ActiveState=active"), false},
		{strings.ReplaceAll(base, "MainPID=0", "MainPID=123"), false},
		{strings.ReplaceAll(base, "Job=0", "Job=123"), false},
		{strings.ReplaceAll(base, "Transient=yes", "Transient=no"), false},
		{strings.ReplaceAll(base, "KillMode=control-group", "KillMode=process"), false},
		{strings.ReplaceAll(base, "DropInPaths=", "DropInPaths=/foreign.conf"), false},
		{base + "MainPID=0\n", false},
	} {
		m.command = func(context.Context, string, []string, []byte) ([]byte, error) { return []byte(tc.body), nil }
		if m.unitStopped(context.Background(), s) != tc.allowed {
			t.Fatal("unit ownership or liveness was guessed")
		}
	}
}

func TestUpgradeClearCannotUseTimeoutAlone(t *testing.T) {
	m := upgradeTestManager(t)
	s := upgradeTestTask(m)
	s.Status = "running"
	s.UpdatedAt = time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339)
	writeUpgradeTestState(t, m, s)
	m.command = func(ctx context.Context, name string, args []string, input []byte) ([]byte, error) {
		if name != "systemctl" {
			t.Fatal("clear attempted a state write without stopped-unit proof")
		}
		return []byte("LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=1\nControlPID=0\nJob=0\n"), nil
	}
	before := upgradeTree(t, m.root)
	response := upgradeResponse(t, m.clearStale, http.MethodPost)
	if response["ok"] != false {
		t.Fatal("stale but active task was cleared")
	}
	assertUpgradeTree(t, m.root, before)
}

func TestUpgradeRendererUsesSharedGuardsAndExactVerification(t *testing.T) {
	m := upgradeTestManager(t)
	s := upgradeTestTask(m)
	info := upgradeRuntime{AppDir: m.root, ConfigDir: filepath.Join(m.root, "config"), Mode: "fast", Version: panelVersion, PID: "777"}
	script, err := m.renderRunner(s, info, "https://manifest.invalid/version.json?channel=testing&v=1")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"set -euo pipefail", "legacy_namespace_preflight", "restore_runtime_preflight", "task_state claim", "create_backup pre-upgrade fast", "validate_upgrade_archive", "FRESH_INSTALL=false", "nginx -t", "health_check", "CODEW_UPDATE_MANIFEST_URL"} {
		if !strings.Contains(script, required) {
			t.Fatalf("runner lost guard: %s", required)
		}
	}
	for _, forbidden := range []string{"systemctl restart", "setsid", "unzip -o", "tar -czf", "recovered_success"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("unsafe old path remained: %s", forbidden)
		}
	}
	if strings.Index(script, "sha256sum -c -") >= strings.Index(script, "validate_upgrade_archive \"$PKG\"") || strings.Index(script, "validate_upgrade_archive \"$PKG\"") >= strings.Index(script, "create_backup pre-upgrade fast") || strings.Index(script, "create_backup pre-upgrade fast") >= strings.Index(script, "unzip -q") {
		t.Fatal("hash, ZIP, backup or extraction order changed")
	}
}
