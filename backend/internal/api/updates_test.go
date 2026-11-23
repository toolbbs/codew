// SPDX-License-Identifier: AGPL-3.0-only
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestComparePanelVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.7.6.0-base-stable-agent-xray", "0.7.5.9.1-qr-flow-compatibility-fix-agent-xray", 1},
		{"0.7.7.1-clash-import-polish-agent-xray", "0.7.6.0-base-stable-agent-xray", 1},
		{"v0.7.7.1", "0.7.7.1-clash-import-polish-agent-xray", 0},
		{"0.7.5.9.1", "0.7.6.0", -1},
		{"0.7.6", "0.7.6.0", 0},
	}
	for _, tc := range cases {
		got := comparePanelVersions(tc.a, tc.b)
		if got < 0 {
			got = -1
		} else if got > 0 {
			got = 1
		}
		if got != tc.want {
			t.Fatalf("comparePanelVersions(%q, %q)=%d want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestUpdateAvailableDoesNotAllowDowngrade(t *testing.T) {
	if isRemoteVersionNewer("0.7.5.9.1-qr-flow-compatibility-fix-agent-xray", "0.7.6.0-base-stable-agent-xray") {
		t.Fatal("older remote version must not be treated as update")
	}
	if !isRemoteVersionNewer("0.7.7.1-clash-import-polish-agent-xray", "0.7.6.0-base-stable-agent-xray") {
		t.Fatal("newer remote version should be treated as update")
	}
}

func codewManifestFixture() updateManifest {
	return updateManifest{
		Version: "0.7.9", Latest: "0.7.9-next-agent-xray", Codename: "next",
		Package: "codew-v0.7.9-next.zip", SHA256: strings.Repeat("a", 64),
		DownloadURL: "https://releases.invalid/codew-v0.7.9-next.zip?download=1&value='$(false)'&literal=`false`",
	}
}

type updateTestTransport func(*http.Request) (*http.Response, error)

func (f updateTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func useUpdateTestClient(t *testing.T, body string) *atomic.Int32 {
	t.Helper()
	previous := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = previous })
	requests := new(atomic.Int32)
	http.DefaultClient = &http.Client{Transport: updateTestTransport(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		if req.Header.Get("User-Agent") != "codew/"+panelVersion {
			t.Error("manifest request did not use codew User-Agent")
		}
		if _, ok := req.Context().Deadline(); !ok {
			t.Error("manifest request has no deadline")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	return requests
}

func TestCodewManifestContract(t *testing.T) {
	if err := validateUpdateManifest(codewManifestFixture()); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*updateManifest)
	}{
		{"empty version", func(m *updateManifest) { m.Version = "" }},
		{"overflow version", func(m *updateManifest) { m.Version = "999999999999999999999999" }},
		{"unicode version", func(m *updateManifest) { m.Version = "\uff10.\uff17.\uff19" }},
		{"missing codename", func(m *updateManifest) { m.Codename = "" }},
		{"unsafe codename", func(m *updateManifest) { m.Codename = "../next" }},
		{"different latest", func(m *updateManifest) { m.Latest = "0.7.10-next" }},
		{"unsafe latest", func(m *updateManifest) { m.Latest = "0.7.9-next;false" }},
		{"old package", func(m *updateManifest) { m.Package = "zxy-panel-v0.7.9-next.zip" }},
		{"unsafe package", func(m *updateManifest) { m.Package = "../codew.zip" }},
		{"missing hash", func(m *updateManifest) { m.SHA256 = "" }},
		{"invalid hash", func(m *updateManifest) { m.SHA256 = strings.Repeat("x", 64) }},
		{"short hash", func(m *updateManifest) { m.SHA256 = strings.Repeat("a", 63) }},
		{"different URL filename", func(m *updateManifest) { m.DownloadURL = "https://releases.invalid/other.zip" }},
		{"http URL", func(m *updateManifest) { m.DownloadURL = "http://releases.invalid/" + m.Package }},
		{"URL credentials", func(m *updateManifest) { m.DownloadURL = "https://user:synthetic-secret@releases.invalid/" + m.Package }},
		{"empty URL credentials", func(m *updateManifest) { m.DownloadURL = "https://@releases.invalid/" + m.Package }},
		{"URL fragment", func(m *updateManifest) { m.DownloadURL += "#" }},
		{"URL whitespace", func(m *updateManifest) { m.DownloadURL += " " }},
		{"URL control", func(m *updateManifest) { m.DownloadURL += "\x7f" }},
		{"URL port", func(m *updateManifest) { m.DownloadURL = "https://releases.invalid:70000/" + m.Package }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := codewManifestFixture()
			tc.change(&manifest)
			err := validateUpdateManifest(manifest)
			if err == nil {
				t.Fatal("invalid manifest was accepted")
			}
			if strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatal("validation error exposed URL credentials")
			}
		})
	}
}

func TestCodewManifestEnvironmentHasNoOldFallback(t *testing.T) {
	t.Setenv("ZXY_UPDATE_MANIFEST_URL", "https://old.invalid/version.json")
	t.Setenv("CODEW_UPDATE_MANIFEST_URL", "")
	if updateManifestURL() != "" {
		t.Fatal("old manifest environment was reused")
	}
	t.Setenv("CODEW_UPDATE_MANIFEST_URL", "https://codew.invalid/version.json?channel=testing&v=1")
	if updateManifestURL() != "https://codew.invalid/version.json?channel=testing&v=1" {
		t.Fatal("new manifest URL was changed")
	}
}

func TestFetchCodewManifestUsesStrictContract(t *testing.T) {
	fixture := codewManifestFixture()
	body, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	requests := useUpdateTestClient(t, string(body))
	got, err := fetchUpdateManifest(context.Background(), "https://manifest.invalid/version.json")
	if err != nil {
		t.Fatal(err)
	}
	if got.Package != fixture.Package || got.DownloadURL != fixture.DownloadURL || got.SHA256 != fixture.SHA256 || got.Codename != fixture.Codename {
		t.Fatal("validated manifest fields changed")
	}
	if requests.Load() != 1 {
		t.Fatal("manifest request was retried")
	}
}

func TestFetchCodewManifestRejectsUnsafeURLBeforeRequest(t *testing.T) {
	requests := useUpdateTestClient(t, "{}")
	for _, value := range []string{"", "http://manifest.invalid/version.json", "https://user:synthetic-secret@manifest.invalid/version.json", "https://manifest.invalid/version.json#", "https://manifest.invalid:70000/version.json"} {
		if _, err := fetchUpdateManifest(context.Background(), value); err == nil {
			t.Fatal("unsafe URL was fetched")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid URL reached the HTTP client")
	}
}

func TestFetchCodewManifestRejectsInvalidJSONAndOversize(t *testing.T) {
	fixture, err := json.Marshal(codewManifestFixture())
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"null", "[]", `{"version":7}`, string(fixture) + "{}", strings.Repeat(" ", updateManifestLimit+1)} {
		t.Run(string(body[:min(len(body), 32)]), func(t *testing.T) {
			useUpdateTestClient(t, body)
			if _, err := fetchUpdateManifest(context.Background(), "https://manifest.invalid/version.json"); err == nil {
				t.Fatal("invalid or oversized manifest was accepted")
			}
		})
	}
}

func TestFetchCodewManifestRedirectCannotDowngradeHTTPS(t *testing.T) {
	fixture, err := json.Marshal(codewManifestFixture())
	if err != nil {
		t.Fatal(err)
	}
	var plainRequests atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		plainRequests.Add(1)
		_, _ = w.Write(fixture)
	}))
	t.Cleanup(plain.Close)
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/redirect":
			http.Redirect(w, req, "/manifest", http.StatusFound)
		case "/downgrade":
			http.Redirect(w, req, plain.URL+"/manifest", http.StatusFound)
		default:
			_, _ = w.Write(fixture)
		}
	}))
	t.Cleanup(secure.Close)
	previous := http.DefaultClient
	http.DefaultClient = secure.Client()
	t.Cleanup(func() { http.DefaultClient = previous })
	if _, err := fetchUpdateManifest(context.Background(), secure.URL+"/redirect"); err != nil {
		t.Fatal(err)
	}
	if _, err := fetchUpdateManifest(context.Background(), secure.URL+"/downgrade"); err == nil {
		t.Fatal("HTTPS to HTTP redirect was accepted")
	}
	if plainRequests.Load() != 0 {
		t.Fatal("downgraded redirect reached the HTTP destination")
	}
}

func TestCodewUpdateAPIRejectsInvalidManifestWithoutCommand(t *testing.T) {
	t.Setenv("CODEW_UPDATE_MANIFEST_URL", "https://manifest.invalid/version.json")
	for _, handler := range []http.HandlerFunc{(&Router{}).updatePanelCommand, (&Router{}).updateCheck} {
		manifest := codewManifestFixture()
		manifest.SHA256 = ""
		body, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		useUpdateTestClient(t, string(body))
		recorder := httptest.NewRecorder()
		handler(recorder, httptest.NewRequest(http.MethodPost, "/api/updates/panel-command", strings.NewReader("{}")))
		var response map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusOK || response["ok"] != false || response["command"] != nil {
			t.Fatal("invalid manifest produced a command or changed the response contract")
		}
	}
}

func TestCodewManualCommandPreservesGuardsAndLiteralURL(t *testing.T) {
	fixture := codewManifestFixture()
	if sha := os.Getenv("CODEW_TEST_FIXTURE_SHA256"); sha != "" {
		fixture.SHA256 = sha
	}
	if err := validateUpdateManifest(fixture); err != nil {
		t.Fatal(err)
	}
	command := manualUpdateCommand(fixture)
	for _, required := range []string{"set -euo pipefail", "source /usr/local/bin/codew", "legacy_namespace_preflight", "load_operational_config", "sha256sum -c -", "validate_upgrade_archive", "FRESH_INSTALL=false", `APP_DIR="$APP_DIR" CONFIG_DIR="$CONFIG_DIR" CODEW_INSTALL_MODE="$MODE"`, "--proto '=https' --proto-redir '=https'", shellEscape(fixture.DownloadURL)} {
		if !strings.Contains(command, strings.ReplaceAll(required, "'", "'\\''")) {
			t.Fatalf("manual command lost required guard: %s", required)
		}
	}
	if !strings.HasPrefix(command, "bash -c '") || strings.Contains(command, "zxy-panel") {
		t.Fatal("manual command has an old namespace or missing shell envelope")
	}
	if strings.Index(command, "sha256sum -c -") >= strings.Index(command, "validate_upgrade_archive \"$package\"") || strings.Index(command, "validate_upgrade_archive \"$package\"") >= strings.Index(command, "unzip -q") || strings.Index(command, "unzip -q") >= strings.Index(command, "bash deploy/install.sh") {
		t.Fatal("hash, archive validation and installation order changed")
	}
	t.Logf("CODEW_MANUAL_COMMAND_B64=%s", base64.StdEncoding.EncodeToString([]byte(command)))
}

func TestCodewManualCommandAPIDoesNotExecuteOrDowngrade(t *testing.T) {
	t.Setenv("CODEW_UPDATE_MANIFEST_URL", "https://manifest.invalid/version.json")
	for _, version := range []string{"0.7.9", "0.7.8", "0.7.8.0", "0.7.7.5"} {
		fixture := codewManifestFixture()
		fixture.Version = version
		fixture.Latest = version + "-next-agent-xray"
		fixture.Package = "codew-v" + version + "-next.zip"
		fixture.DownloadURL = "https://releases.invalid/" + fixture.Package
		body, err := json.Marshal(fixture)
		if err != nil {
			t.Fatal(err)
		}
		requests := useUpdateTestClient(t, string(body))
		recorder := httptest.NewRecorder()
		(&Router{}).updatePanelCommand(recorder, httptest.NewRequest(http.MethodPost, "/api/updates/panel-command", strings.NewReader("{}")))
		var response map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusOK || requests.Load() != 1 {
			t.Fatal("command generation changed HTTP contract or made extra network requests")
		}
		if version == "0.7.9" {
			if response["ok"] != true || response["command"] != manualUpdateCommand(fixture) {
				t.Fatal("newer validated release did not produce the reviewed command")
			}
		} else if response["ok"] != false || response["command"] != nil {
			t.Fatal("same or older release produced an installation command")
		}
	}
}
