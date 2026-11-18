package python

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// ensureUV 返回可用的 uv 可执行文件；找不到就下载到 DataDir（优先从 PyPI 镜像下载 uv 的 wheel，失败再用 GitHub release）。
func (p *Plugin) ensureUV(ctx *sdk.Context, console string) (string, error) {
	p.uvMu.Lock()
	defer p.uvMu.Unlock()
	if p.uv != "" && sdk.FileExists(p.uv) {
		return p.uv, nil
	}
	if u := findUV(ctx); u != "" {
		p.uv = u
		return u, nil
	}
	if ctx.Init == nil || ctx.Init.DataDir == "" {
		return "", sdk.UserError("%s", ctx.T("err.uvMissing"))
	}
	ctx.Host.Console(console, ctx.T("step.downloadUV"))
	u, err := p.downloadUV(ctx, console)
	if err != nil {
		return "", sdk.UserError("%s", ctx.T("err.uvDownload", err.Error()))
	}
	p.uv = u
	return u, nil
}

// findUV 在 PATH 与常见安装位置找 uv（图形界面拉起的 daemon 的 PATH 往往很短，所以要显式检查）。
func findUV(ctx *sdk.Context) string {
	name := sdk.ExeName("uv")
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	home := sdk.HomeDir()
	cands := []string{
		filepath.Join(home, ".local", "bin", name),
		filepath.Join(home, ".cargo", "bin", name),
	}
	switch runtime.GOOS {
	case "darwin":
		cands = append(cands, "/opt/homebrew/bin/uv", "/usr/local/bin/uv")
	case "linux":
		cands = append(cands, "/usr/local/bin/uv", "/usr/bin/uv")
	case "windows":
		cands = append(cands, filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "uv", name), filepath.Join(home, "scoop", "shims", name))
	}
	if ctx != nil && ctx.Init != nil && ctx.Init.DataDir != "" {
		cands = append(cands, filepath.Join(ctx.Init.DataDir, "uv", name))
	}
	for _, c := range cands {
		if sdk.FileExists(c) {
			return c
		}
	}
	return ""
}

var wheelLinkRe = regexp.MustCompile(`href="([^"]+/(uv-(\d+\.\d+\.\d+)-py3-none-([^"#]+)\.whl)(?:#sha256=([0-9a-f]{64}))?)"`)

// uvWheelTag 是当前平台 uv wheel 文件名里的平台标记片段。
func uvWheelTag() (must []string, ok bool) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return []string{"macosx", "arm64"}, true
	case "darwin/amd64":
		return []string{"macosx", "x86_64"}, true
	case "linux/amd64":
		return []string{"manylinux", "x86_64"}, true
	case "linux/arm64":
		return []string{"manylinux", "aarch64"}, true
	case "windows/amd64":
		return []string{"win_amd64"}, true
	case "windows/arm64":
		return []string{"win_arm64"}, true
	}
	return nil, false
}

// uvReleaseTarget 是 GitHub release 的目标三元组。
func uvReleaseTarget() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "aarch64-apple-darwin"
	case "darwin/amd64":
		return "x86_64-apple-darwin"
	case "linux/amd64":
		return "x86_64-unknown-linux-gnu"
	case "linux/arm64":
		return "aarch64-unknown-linux-gnu"
	case "windows/amd64":
		return "x86_64-pc-windows-msvc"
	case "windows/arm64":
		return "aarch64-pc-windows-msvc"
	}
	return ""
}

func (p *Plugin) downloadUV(ctx *sdk.Context, console string) (string, error) {
	dest := filepath.Join(ctx.Init.DataDir, "uv")
	tmp := dest + ".tmp"
	_ = os.RemoveAll(tmp)
	name := sdk.ExeName("uv")
	var firstErr error
	// 1. PyPI（镜像）上的 uv wheel
	if must, ok := uvWheelTag(); ok {
		index := p.mirrors(ctx).PypiIndex
		if index == "" {
			index = "https://pypi.org/simple/"
		}
		wurl, sha, err := latestUVWheel(ctx, index, must)
		if err == nil {
			_, err = ctx.Host.Download(ctx, &sdk.DownloadParams{URLs: []string{wurl}, SHA256: sha, Dest: tmp, Extract: "zip", TaskID: console, Title: "uv"})
		}
		if err == nil {
			if bin := findFile(tmp, name); bin != "" {
				return installUVBinary(bin, dest, name, tmp)
			}
			err = fmt.Errorf("uv binary not found in wheel")
		}
		firstErr = err
	}
	// 2. GitHub release
	if target := uvReleaseTarget(); target != "" {
		ext, kind := ".tar.gz", "tar.gz"
		if runtime.GOOS == "windows" {
			ext, kind = ".zip", "zip"
		}
		u := "https://github.com/astral-sh/uv/releases/latest/download/uv-" + target + ext
		_ = os.RemoveAll(tmp)
		if _, err := ctx.Host.Download(ctx, &sdk.DownloadParams{URLs: []string{u}, Dest: tmp, Extract: kind, TaskID: console, Title: "uv"}); err == nil {
			if bin := findFile(tmp, name); bin != "" {
				return installUVBinary(bin, dest, name, tmp)
			}
		} else if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return "", firstErr
}

func installUVBinary(bin, dest, name, tmp string) (string, error) {
	_ = os.MkdirAll(dest, 0o755)
	target := filepath.Join(dest, name)
	_ = os.Remove(target)
	if err := os.Rename(bin, target); err != nil {
		return "", err
	}
	// uvx 一起放进来（wheel 与 release 里都有）
	if x := findFile(tmp, sdk.ExeName("uvx")); x != "" {
		_ = os.Rename(x, filepath.Join(dest, sdk.ExeName("uvx")))
		_ = os.Chmod(filepath.Join(dest, sdk.ExeName("uvx")), 0o755)
	}
	_ = os.Chmod(target, 0o755)
	_ = os.RemoveAll(tmp)
	return target, nil
}

// latestUVWheel 读 PEP 503 简单索引页，挑当前平台最新的稳定版 uv wheel。
func latestUVWheel(ctx context.Context, index string, must []string) (string, string, error) {
	base := strings.TrimSuffix(index, "/") + "/uv/"
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(c, http.MethodGet, base, nil)
	req.Header.Set("Accept", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("%s: HTTP %d", base, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", "", err
	}
	type wheel struct{ href, version, sha string }
	var ws []wheel
	for _, m := range wheelLinkRe.FindAllStringSubmatch(string(body), -1) {
		tag := m[4]
		ok := true
		for _, s := range must {
			if !strings.Contains(tag, s) {
				ok = false
			}
		}
		if ok && !strings.Contains(tag, "musllinux") {
			ws = append(ws, wheel{href: m[1], version: m[3], sha: m[5]})
		}
	}
	if len(ws) == 0 {
		return "", "", fmt.Errorf("no uv wheel for this platform at %s", base)
	}
	sort.SliceStable(ws, func(i, j int) bool { return sdk.CompareVersions(ws[i].version, ws[j].version) > 0 })
	w := ws[0]
	href := strings.ReplaceAll(w.href, "&amp;", "&")
	bu, _ := url.Parse(base)
	ru, err := url.Parse(href)
	if err != nil {
		return "", "", err
	}
	return bu.ResolveReference(ru).String(), w.sha, nil
}

// findFile 在目录树里找指定文件名。
func findFile(root, name string) string {
	var found string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if !info.IsDir() && info.Name() == name {
			found = path
		}
		return nil
	})
	return found
}
