// SPDX-License-Identifier: AGPL-3.0-only
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const updateManifestLimit = 262144

var errUnsafeUpdateURL = errors.New("update URL must use HTTPS without credentials, fragments or invalid characters")

type updateManifest struct {
	Latest              string   `json:"latest"`
	Version             string   `json:"version"`
	Codename            string   `json:"codename"`
	Package             string   `json:"package"`
	DownloadURL         string   `json:"download_url"`
	SHA256              string   `json:"sha256"`
	Changelog           []string `json:"changelog"`
	MinSupportedVersion string   `json:"min_supported_version"`
}

func updateManifestURL() string {
	return getenv("CODEW_UPDATE_MANIFEST_URL", "")
}

func (r *Router) updateStatus(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	manifestURL := updateManifestURL()
	configured := manifestURL != ""
	note := "未配置远程版本清单。请先把代码仓库和 version.json 发布好，再通过环境变量 CODEW_UPDATE_MANIFEST_URL 配置正式地址。"
	if configured {
		note = "已配置远程版本清单，可检查更新并生成升级命令。"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current_version":     panelVersion,
		"manifest_url":        updateManifestDisplayURL(manifestURL),
		"manifest_configured": configured,
		"manifest_display":    firstNonEmpty(updateManifestDisplayURL(manifestURL), "未配置"),
		"xray_version":        r.currentXrayVersionText(),
		"install_dir":         installDir,
		"backup_dir":          filepath.Join(installDir, "backups"),
		"note":                note,
	})
}

func (r *Router) updateCheck(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost && req.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	manifestURL := updateManifestURL()
	if manifestURL == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":              false,
			"current_version": panelVersion,
			"manifest_url":    "",
			"message":         "暂未配置远程版本清单。请先上传代码仓库并发布 version.json，再设置 CODEW_UPDATE_MANIFEST_URL。",
		})
		return
	}
	manifest, err := fetchUpdateManifest(req.Context(), manifestURL)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":              false,
			"current_version": panelVersion,
			"manifest_url":    updateManifestDisplayURL(manifestURL),
			"error":           err.Error(),
			"message":         "暂时无法获取远程版本清单：" + err.Error(),
		})
		return
	}
	latest := manifest.Latest
	if latest == "" {
		latest = manifest.Version
	}
	available := isRemoteVersionNewer(latest, panelVersion)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"current_version":  panelVersion,
		"latest_version":   latest,
		"update_available": available,
		"manifest":         manifest,
		"message":          updateMessage(latest),
	})
}

func (r *Router) updatePanelCommand(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost && req.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	manifestURL := updateManifestURL()
	if manifestURL == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "无法生成升级命令：远程版本清单未配置。请先发布 version.json 并设置 CODEW_UPDATE_MANIFEST_URL。"})
		return
	}
	manifest, err := fetchUpdateManifest(req.Context(), manifestURL)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "message": "无法生成升级命令：远程版本清单不可用：" + err.Error()})
		return
	}
	target := firstNonEmpty(manifest.Latest, manifest.Version)
	if !isRemoteVersionNewer(target, panelVersion) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "current_version": panelVersion, "target_version": target, "message": "当前已是最新版本，远程版本不高于当前版本，已禁止生成降级命令。"})
		return
	}
	command := manualUpdateCommand(manifest)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"current_version": panelVersion,
		"target_version":  firstNonEmpty(manifest.Latest, manifest.Version),
		"package":         manifest.Package,
		"command":         command,
		"message":         "已生成供审核的升级命令，尚未执行安装。请在原宿主机确认备份和快照后执行；此命令不提供自动整版回滚。",
	})
}

func manualUpdateCommand(manifest updateManifest) string {
	root := strings.TrimSuffix(manifest.Package, ".zip")
	script := strings.Join([]string{
		"set -euo pipefail",
		"[[ -f /usr/local/bin/codew && ! -L /usr/local/bin/codew ]] || { echo 'ERROR: installed codew CLI is unavailable.' >&2; exit 1; }",
		"unset CONFIG_DIR",
		"source /usr/local/bin/codew",
		"declare -F validate_upgrade_archive >/dev/null || { echo 'ERROR: installed codew CLI lacks update safeguards.' >&2; exit 1; }",
		"legacy_namespace_preflight",
		"load_operational_config",
		"[[ \"$EXPECTED_VERSION\" == " + shellEscape(panelVersion) + " ]] || { echo 'ERROR: installation changed; regenerate the upgrade command.' >&2; exit 1; }",
		"mkdir -p /root/codew-upgrades",
		"work=$(mktemp -d /root/codew-upgrades/job-XXXXXX)",
		"cd \"$work\"",
		"package=" + shellEscape(manifest.Package),
		"curl --fail --location --silent --show-error --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 300 -o \"$package\" " + shellEscape(manifest.DownloadURL),
		"printf '%s  %s\\n' " + shellEscape(strings.ToLower(manifest.SHA256)) + " \"$package\" | sha256sum -c -",
		"validate_upgrade_archive \"$package\" " + shellEscape(root),
		"unzip -q \"$package\"",
		"cd " + shellEscape(root),
		"APP_DIR=\"$APP_DIR\" CONFIG_DIR=\"$CONFIG_DIR\" CODEW_INSTALL_MODE=\"$MODE\" FRESH_INSTALL=false bash deploy/install.sh 2>&1 | tee /root/codew-upgrade.log",
	}, "\n")
	return "bash -c " + shellEscape(script)
}

func (r *Router) updateXrayStatus(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current_xray":   r.currentXrayVersionText(),
		"message":        "网络核心升级入口已预留。下一步会加入检查最新版、备份旧核心、测试配置、替换并重启。",
		"planned_checks": []string{"备份当前 xray", "下载新版网络核心", "执行配置测试", "测试通过后重启", "失败保留旧核心"},
	})
}

func validUpdateURL(value string) (*url.URL, error) {
	if strings.Contains(value, "#") || strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return nil, errUnsafeUpdateURL
	}
	parsed, err := url.Parse(value)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Hostname() == "" || parsed.User != nil {
		return nil, errUnsafeUpdateURL
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return nil, errUnsafeUpdateURL
		}
	}
	return parsed, nil
}

func updateManifestDisplayURL(value string) string {
	if value == "" {
		return ""
	}
	if _, err := validUpdateURL(value); err != nil {
		return "无效清单地址"
	}
	return value
}

func validateUpdateManifest(manifest updateManifest) error {
	if !regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)*$`).MatchString(manifest.Version) {
		return fmt.Errorf("invalid manifest version")
	}
	for _, part := range strings.Split(manifest.Version, ".") {
		if _, err := strconv.Atoi(part); err != nil {
			return fmt.Errorf("invalid manifest version")
		}
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(manifest.Codename) {
		return fmt.Errorf("invalid manifest codename")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(manifest.Latest) || !strings.HasPrefix(manifest.Latest, manifest.Version+"-") {
		return fmt.Errorf("invalid manifest latest version")
	}
	if manifest.Package != "codew-v"+manifest.Version+"-"+manifest.Codename+".zip" {
		return fmt.Errorf("invalid codew package name")
	}
	if _, err := hex.DecodeString(manifest.SHA256); err != nil || len(manifest.SHA256) != 64 {
		return fmt.Errorf("invalid manifest SHA256")
	}
	parsed, err := validUpdateURL(manifest.DownloadURL)
	if err != nil {
		return err
	}
	if parsed.Path[strings.LastIndex(parsed.Path, "/")+1:] != manifest.Package {
		return fmt.Errorf("package and download URL do not match")
	}
	return nil
}

func fetchUpdateManifest(ctx context.Context, manifestURL string) (updateManifest, error) {
	var manifest updateManifest
	if _, err := validUpdateURL(manifestURL); err != nil {
		return manifest, err
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return manifest, fmt.Errorf("invalid manifest request")
	}
	req.Header.Set("User-Agent", "codew/"+panelVersion)
	client := *http.DefaultClient
	previousRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if _, err := validUpdateURL(req.URL.String()); err != nil {
			return err
		}
		if len(via) >= 10 {
			return fmt.Errorf("manifest redirect limit exceeded")
		}
		if previousRedirect != nil {
			return previousRedirect(req, via)
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errUnsafeUpdateURL) {
			return manifest, errUnsafeUpdateURL
		}
		if cctx.Err() != nil {
			return manifest, fmt.Errorf("manifest request cancelled or timed out")
		}
		return manifest, fmt.Errorf("manifest request failed (network, TLS or redirect error)")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return manifest, fmt.Errorf("version manifest http status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, updateManifestLimit+1))
	if err != nil {
		return manifest, fmt.Errorf("manifest response could not be read")
	}
	if len(body) > updateManifestLimit {
		return manifest, fmt.Errorf("manifest response exceeds size limit")
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return manifest, fmt.Errorf("invalid manifest JSON")
	}
	if err := validateUpdateManifest(manifest); err != nil {
		return updateManifest{}, err
	}
	return manifest, nil
}

func (r *Router) currentXrayVersionText() string {
	r.store.Mu.RLock()
	version := ""
	for _, srv := range r.store.Data.Servers {
		if strings.TrimSpace(srv.XrayVersion) != "" {
			version = strings.TrimSpace(srv.XrayVersion)
			break
		}
	}
	r.store.Mu.RUnlock()
	if version != "" {
		return version
	}
	return currentXrayVersionText()
}

func currentXrayVersionText() string {
	candidates := []string{"xray", "/usr/local/bin/xray", "/usr/bin/xray"}
	for _, bin := range candidates {
		if strings.Contains(bin, "/") {
			if _, err := os.Stat(bin); err != nil {
				continue
			}
		}
		out, err := exec.Command(bin, "version").CombinedOutput()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			return strings.TrimSpace(lines[0])
		}
	}
	return "未检测到 Xray 或当前容器不可访问宿主机 Xray"
}

func updateMessage(latest string) string {
	if latest == "" {
		return "远程版本清单未提供 latest 字段。"
	}
	if isRemoteVersionNewer(latest, panelVersion) {
		return "发现新版本，可查看更新日志并生成升级命令。"
	}
	return "当前已是最新版本。"
}

func isRemoteVersionNewer(remote, current string) bool {
	return comparePanelVersions(remote, current) > 0
}

func comparePanelVersions(a, b string) int {
	ap := numericVersionParts(a)
	bp := numericVersionParts(b)
	max := len(ap)
	if len(bp) > max {
		max = len(bp)
	}
	for i := 0; i < max; i++ {
		ai, bi := 0, 0
		if i < len(ap) {
			ai = ap[i]
		}
		if i < len(bp) {
			bi = bp[i]
		}
		if ai > bi {
			return 1
		}
		if ai < bi {
			return -1
		}
	}
	return 0
}

func numericVersionParts(s string) []int {
	s = strings.TrimSpace(s)
	start := -1
	for i, r := range s {
		if r >= '0' && r <= '9' {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	end := start
	for end < len(s) {
		c := s[end]
		if (c < '0' || c > '9') && c != '.' {
			break
		}
		end++
	}
	parts := strings.Split(strings.Trim(s[start:end], "."), ".")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			out = append(out, 0)
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			n = 0
		}
		out = append(out, n)
	}
	return out
}

func shellEscape(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
