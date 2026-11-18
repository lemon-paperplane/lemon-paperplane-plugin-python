package python

import (
	"context"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/json"
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

// 工具链来源。
const (
	srcUV        = "uv"
	srcPyenv     = "pyenv"
	srcConda     = "conda"
	srcHomebrew  = "homebrew"
	srcPythonOrg = "python.org"
	srcSystem    = "system"
	srcManaged   = "managed"
	srcAsdf      = "asdf"
	srcMise      = "mise"
)

// candidate 是一个待检查的 Python 安装前缀。
type candidate struct {
	prefix    string // 安装根
	exe       string // 解释器（空则自动推断）
	source    string
	label     string // 来源附加说明，如 conda 环境名
	extraPath []string
}

// DiscoverToolchains 扫描已知安装位置，只读。
func (p *Plugin) DiscoverToolchains(ctx *sdk.Context, d *sdk.DiscoverToolchainsParams) (*sdk.DiscoverToolchainsResult, error) {
	if d.Kind != "" && d.Kind != "python" {
		return nil, sdk.ErrNotSupported
	}
	var cands []candidate
	if len(d.Paths) > 0 {
		for _, path := range d.Paths {
			cands = append(cands, candidatesAt(path, srcManaged)...)
		}
	} else {
		cands = knownCandidates(ctx)
	}
	seen := map[string]bool{}
	var out []sdk.Toolchain
	for _, c := range cands {
		tc, ok := inspectCandidate(ctx, c)
		if !ok {
			continue
		}
		key := tc.Executable
		if real, err := filepath.EvalSymlinks(tc.Executable); err == nil {
			key = real
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, tc)
	}
	sort.SliceStable(out, func(i, j int) bool { return sdk.CompareVersions(out[i].Version, out[j].Version) > 0 })
	return &sdk.DiscoverToolchainsResult{Toolchains: out}, nil
}

// candidatesAt 处理用户指定或刚安装好的目录：可能是前缀本身，也可能是 uv 的安装目录（下面一层 cpython-*）。
func candidatesAt(path, source string) []candidate {
	path = sdk.Expand(path)
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		// 直接给了解释器文件
		return []candidate{{prefix: prefixOfExe(path), exe: path, source: source}}
	}
	if pythonExe(path) != "" {
		return []candidate{{prefix: path, source: source}}
	}
	var out []candidate
	ents, _ := os.ReadDir(path)
	for _, e := range ents {
		sub := filepath.Join(path, e.Name())
		if pythonExe(sub) != "" {
			out = append(out, candidate{prefix: sub, source: source})
		}
	}
	return out
}

// prefixOfExe 由解释器路径推出安装前缀（bin/python3 → 上一级）。
func prefixOfExe(exe string) string {
	dir := filepath.Dir(exe)
	if runtime.GOOS != "windows" && filepath.Base(dir) == "bin" {
		return filepath.Dir(dir)
	}
	return dir
}

// pythonExe 返回前缀下的解释器路径，不存在返回空。
func pythonExe(prefix string) string {
	var names []string
	if runtime.GOOS == "windows" {
		names = []string{"python.exe"}
		for _, n := range names {
			if p := filepath.Join(prefix, n); sdk.FileExists(p) {
				return p
			}
		}
		return ""
	}
	names = []string{"python3", "python"}
	for _, n := range names {
		if p := filepath.Join(prefix, "bin", n); sdk.FileExists(p) {
			return p
		}
	}
	// 只有带版本号的可执行文件（如 bin/python3.12）
	ents, _ := os.ReadDir(filepath.Join(prefix, "bin"))
	for _, e := range ents {
		if pyVerExeRe.MatchString(e.Name()) {
			return filepath.Join(prefix, "bin", e.Name())
		}
	}
	return ""
}

// knownCandidates 列出各平台已知安装位置。
func knownCandidates(ctx *sdk.Context) []candidate {
	home := sdk.HomeDir()
	var out []candidate
	add := func(prefix, source, label string) {
		out = append(out, candidate{prefix: prefix, source: source, label: label})
	}
	subdirs := func(dir string) []string {
		ents, _ := os.ReadDir(dir)
		var res []string
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				res = append(res, p)
			}
		}
		return res
	}

	// Paperplane 自己装的
	if ctx != nil && ctx.Init != nil && ctx.Init.HostDataDir != "" {
		for _, d := range subdirs(filepath.Join(ctx.Init.HostDataDir, "toolchains", "python")) {
			for _, c := range candidatesAt(d, srcManaged) {
				out = append(out, c)
			}
		}
	}
	// uv 管理的
	for _, d := range uvPythonDirs() {
		for _, sub := range subdirs(d) {
			// uv 0.8+ 会建 cpython-3.12-… 这种指向具体版本的次版本链接，跳过链接只取真实目录
			if st, err := os.Lstat(sub); err == nil && st.Mode()&os.ModeSymlink != 0 {
				continue
			}
			if strings.HasPrefix(filepath.Base(sub), "cpython-") || strings.HasPrefix(filepath.Base(sub), "pypy-") {
				add(sub, srcUV, "")
			}
		}
	}
	// pyenv
	pyenvRoot := os.Getenv("PYENV_ROOT")
	if pyenvRoot == "" {
		pyenvRoot = filepath.Join(home, ".pyenv")
	}
	if runtime.GOOS == "windows" {
		for _, d := range subdirs(filepath.Join(pyenvRoot, "pyenv-win", "versions")) {
			add(d, srcPyenv, "")
		}
	} else {
		for _, d := range subdirs(filepath.Join(pyenvRoot, "versions")) {
			add(d, srcPyenv, "")
		}
	}
	// asdf / mise
	for _, d := range subdirs(filepath.Join(home, ".asdf", "installs", "python")) {
		add(d, srcAsdf, "")
	}
	for _, d := range subdirs(filepath.Join(home, ".local", "share", "mise", "installs", "python")) {
		add(d, srcMise, "")
	}
	// conda：base 与 envs/*
	for _, root := range condaRoots() {
		add(root, srcConda, "base")
		for _, env := range subdirs(filepath.Join(root, "envs")) {
			add(env, srcConda, filepath.Base(env))
		}
	}
	for _, env := range subdirs(filepath.Join(home, ".conda", "envs")) {
		add(env, srcConda, filepath.Base(env))
	}

	switch runtime.GOOS {
	case "darwin":
		for _, brew := range []string{"/opt/homebrew/opt", "/usr/local/opt"} {
			ents, _ := os.ReadDir(brew)
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), "python@3.") {
					p := filepath.Join(brew, e.Name())
					out = append(out, candidate{prefix: p, source: srcHomebrew, extraPath: []string{filepath.Join(p, "libexec", "bin")}})
				}
			}
		}
		for _, d := range subdirs("/Library/Frameworks/Python.framework/Versions") {
			if filepath.Base(d) != "Current" {
				add(d, srcPythonOrg, "")
			}
		}
		// Xcode / 命令行工具自带的 Python（直接读框架目录，不调用 /usr/bin/python3 桩）
		for _, fw := range []string{
			"/Applications/Xcode.app/Contents/Developer/Library/Frameworks/Python3.framework/Versions",
			"/Library/Developer/CommandLineTools/Library/Frameworks/Python3.framework/Versions",
		} {
			for _, d := range subdirs(fw) {
				if filepath.Base(d) != "Current" {
					add(d, srcSystem, "Xcode")
				}
			}
		}
	case "linux":
		for _, dir := range []string{"/usr/bin", "/usr/local/bin"} {
			ents, _ := os.ReadDir(dir)
			for _, e := range ents {
				if pyVerExeRe.MatchString(e.Name()) {
					exe := filepath.Join(dir, e.Name())
					out = append(out, candidate{prefix: filepath.Dir(dir), exe: exe, source: srcSystem})
				}
			}
		}
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		for _, base := range []string{filepath.Join(local, "Programs", "Python"), `C:\Program Files`, `C:\Program Files (x86)`, `C:\`} {
			ents, _ := os.ReadDir(base)
			for _, e := range ents {
				if strings.HasPrefix(strings.ToLower(e.Name()), "python3") || strings.HasPrefix(e.Name(), "Python3") {
					add(filepath.Join(base, e.Name()), srcPythonOrg, "")
				}
			}
		}
		for _, d := range subdirs(filepath.Join(home, "scoop", "apps", "python")) {
			if filepath.Base(d) != "current" {
				add(d, "scoop", "")
			}
		}
		out = append(out, pyLauncherCandidates()...)
	}
	// PATH 上的 python3 / python（排除 macOS 的 /usr/bin/python3 桩与 Windows 应用商店别名）
	for _, n := range []string{"python3", "python"} {
		exe, err := exec.LookPath(n)
		if err != nil {
			continue
		}
		if runtime.GOOS == "darwin" && exe == "/usr/bin/python3" {
			continue
		}
		if runtime.GOOS == "windows" && strings.Contains(strings.ToLower(exe), `\microsoft\windowsapps\`) {
			continue
		}
		real, err := filepath.EvalSymlinks(exe)
		if err != nil {
			continue
		}
		out = append(out, candidate{prefix: prefixOfExe(real), exe: real, source: srcSystem})
	}
	return out
}

// pyLauncherCandidates 用 Windows 的 py 启动器列出已注册的解释器（py -0p）。
func pyLauncherCandidates() []candidate {
	if runtime.GOOS != "windows" {
		return nil
	}
	c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "py", "-0p").Output()
	if err != nil {
		return nil
	}
	var res []candidate
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		exe := f[len(f)-1]
		if strings.HasSuffix(strings.ToLower(exe), ".exe") && sdk.FileExists(exe) {
			res = append(res, candidate{prefix: filepath.Dir(exe), exe: exe, source: srcPythonOrg})
		}
	}
	return res
}

// uvPythonDirs 返回 uv 管理 Python 的目录（UV_PYTHON_INSTALL_DIR 或默认位置）。
func uvPythonDirs() []string {
	var out []string
	if d := os.Getenv("UV_PYTHON_INSTALL_DIR"); d != "" {
		out = append(out, d)
	}
	switch runtime.GOOS {
	case "windows":
		out = append(out, filepath.Join(os.Getenv("APPDATA"), "uv", "python"))
	default:
		base := os.Getenv("XDG_DATA_HOME")
		if base == "" {
			base = filepath.Join(sdk.HomeDir(), ".local", "share")
		}
		out = append(out, filepath.Join(base, "uv", "python"))
	}
	return out
}

// condaRoots 列出已知的 conda 安装根目录（只看目录，不跑 conda 命令）。
func condaRoots() []string {
	home := sdk.HomeDir()
	names := []string{"anaconda3", "miniconda3", "miniforge3", "mambaforge", "micromamba", "anaconda", "miniconda"}
	var cands []string
	if p := os.Getenv("CONDA_ROOT"); p != "" {
		cands = append(cands, p)
	}
	if exe := os.Getenv("CONDA_EXE"); exe != "" {
		cands = append(cands, filepath.Dir(filepath.Dir(exe)))
	}
	for _, n := range names {
		cands = append(cands, filepath.Join(home, n), filepath.Join(home, "opt", n))
		switch runtime.GOOS {
		case "windows":
			cands = append(cands, filepath.Join(`C:\ProgramData`, n), filepath.Join(os.Getenv("LOCALAPPDATA"), n))
		default:
			cands = append(cands, filepath.Join("/opt", n), filepath.Join("/usr/local", n))
		}
	}
	if runtime.GOOS == "darwin" {
		for _, cask := range []string{"/opt/homebrew/Caskroom", "/usr/local/Caskroom"} {
			for _, n := range []string{"miniforge", "miniconda", "anaconda", "mambaforge"} {
				ents, _ := os.ReadDir(filepath.Join(cask, n))
				for _, e := range ents {
					for _, inner := range names {
						cands = append(cands, filepath.Join(cask, n, e.Name(), inner))
					}
					cands = append(cands, filepath.Join(cask, n, "base"))
				}
			}
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range cands {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		if sdk.DirExists(filepath.Join(c, "conda-meta")) {
			out = append(out, c)
		}
	}
	return out
}

// inspectCandidate 确认候选可用并读出版本、架构。
func inspectCandidate(ctx *sdk.Context, c candidate) (sdk.Toolchain, bool) {
	prefix := c.prefix
	exe := c.exe
	if exe == "" {
		exe = pythonExe(prefix)
	}
	if exe == "" || !sdk.FileExists(exe) {
		return sdk.Toolchain{}, false
	}
	// 虚拟环境不是工具链
	if sdk.FileExists(filepath.Join(prefix, "pyvenv.cfg")) {
		return sdk.Toolchain{}, false
	}
	version, impl := versionFromFiles(prefix, exe, c.source)
	if version == "" {
		version, impl = versionByRunning(exe)
	}
	if version == "" {
		return sdk.Toolchain{}, false
	}
	tc := sdk.Toolchain{
		Kind:       "python",
		Version:    version,
		Vendor:     impl,
		Arch:       exeArch(exe),
		Path:       prefix,
		Executable: exe,
		Source:     c.source,
		Managed:    c.source == srcManaged,
	}
	if runtime.GOOS == "windows" {
		tc.PathPrepend = []string{filepath.Dir(exe), filepath.Join(filepath.Dir(exe), "Scripts")}
		if c.source == srcConda {
			tc.PathPrepend = append(tc.PathPrepend, filepath.Join(prefix, "Library", "bin"))
		}
	} else {
		tc.PathPrepend = append(append([]string{}, c.extraPath...), filepath.Dir(exe))
	}
	label := "Python " + version
	switch c.source {
	case srcConda:
		label += " · conda"
		if c.label != "" {
			label += " (" + c.label + ")"
		}
	case srcSystem:
		if c.label != "" {
			label += " · " + c.label
		} else {
			label += " · " + srcSystem
		}
	case srcHomebrew:
		label += " · Homebrew"
	case srcManaged:
		label += " · Paperplane"
	case "":
	default:
		label += " · " + c.source
	}
	if impl != "" && impl != "CPython" {
		label = strings.Replace(label, "Python", impl, 1)
	}
	tc.Label = label
	return tc, true
}

var (
	patchlevelRe = regexp.MustCompile(`#define\s+PY_VERSION\s+"([^"]+)"`)
	uvDirRe      = regexp.MustCompile(`^(cpython|pypy|graalpy)-(\d+\.\d+\.\d+[a-z0-9]*)(?:\+[a-z]+)?-`)
	cellarRe     = regexp.MustCompile(`/Cellar/python@[\d.]+/(\d+\.\d+\.\d+)`)
	pyVerExeRe   = regexp.MustCompile(`^python3\.\d+$`)
)

// versionFromFiles 尽量从文件得到版本（不执行解释器）。
func versionFromFiles(prefix, exe, source string) (string, string) {
	base := filepath.Base(prefix)
	if m := uvDirRe.FindStringSubmatch(base); m != nil {
		impl := map[string]string{"cpython": "CPython", "pypy": "PyPy", "graalpy": "GraalPy"}[m[1]]
		return m[2], impl
	}
	if source == srcConda {
		if v := condaPythonVersion(prefix); v != "" {
			return v, "CPython"
		}
	}
	if real, err := filepath.EvalSymlinks(prefix); err == nil {
		if m := cellarRe.FindStringSubmatch(filepath.ToSlash(real)); m != nil {
			return m[1], "CPython"
		}
	}
	// include/python3.X/patchlevel.h（Homebrew 在 Frameworks 下）
	incDirs := []string{filepath.Join(prefix, "include"), filepath.Join(prefix, "Headers")}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		incDirs = append(incDirs, filepath.Join(prefixOfExe(real), "include"))
	}
	for _, inc := range incDirs {
		if v := patchlevel(filepath.Join(inc, "patchlevel.h")); v != "" {
			return v, "CPython"
		}
		ents, _ := os.ReadDir(inc)
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "python3") {
				if v := patchlevel(filepath.Join(inc, e.Name(), "patchlevel.h")); v != "" {
					return v, "CPython"
				}
			}
		}
	}
	return "", ""
}

func patchlevel(path string) string {
	if m := patchlevelRe.FindStringSubmatch(sdk.ReadText(path, 64<<10)); m != nil {
		return m[1]
	}
	return ""
}

// versionByRunning 执行解释器取版本（兜底，2 秒超时）。
func versionByRunning(exe string) (string, string) {
	c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, exe, "-c", "import sys,platform;print(platform.python_version());print(platform.python_implementation())").Output()
	if err != nil {
		return "", ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return "", ""
	}
	return strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
}

// exeArch 读可执行文件头判断架构（Go 的 GOARCH 命名）；通用二进制返回当前架构。
func exeArch(exe string) string {
	switch runtime.GOOS {
	case "darwin":
		if f, err := macho.Open(exe); err == nil {
			defer f.Close()
			return machoArch(f.Cpu)
		}
		if ff, err := macho.OpenFat(exe); err == nil {
			defer ff.Close()
			for _, a := range ff.Arches {
				if machoArch(a.Cpu) == runtime.GOARCH {
					return runtime.GOARCH
				}
			}
			if len(ff.Arches) > 0 {
				return machoArch(ff.Arches[0].Cpu)
			}
		}
	case "linux":
		if f, err := elf.Open(exe); err == nil {
			defer f.Close()
			switch f.Machine {
			case elf.EM_X86_64:
				return "amd64"
			case elf.EM_AARCH64:
				return "arm64"
			case elf.EM_386:
				return "386"
			}
		}
	case "windows":
		if f, err := pe.Open(exe); err == nil {
			defer f.Close()
			switch f.Machine {
			case pe.IMAGE_FILE_MACHINE_AMD64:
				return "amd64"
			case pe.IMAGE_FILE_MACHINE_ARM64:
				return "arm64"
			case pe.IMAGE_FILE_MACHINE_I386:
				return "386"
			}
		}
	}
	return ""
}

func machoArch(c macho.Cpu) string {
	switch c {
	case macho.CpuArm64:
		return "arm64"
	case macho.CpuAmd64:
		return "amd64"
	}
	return ""
}

// ---------------------------------------------------------------------------
// available / install（交给 uv）

// AvailableToolchains 列出 uv 可下载的 CPython 版本（每个次版本取最新补丁）。
func (p *Plugin) AvailableToolchains(ctx *sdk.Context, a *sdk.AvailableToolchainsParams) (*sdk.AvailableToolchainsResult, error) {
	if a.Kind != "" && a.Kind != "python" {
		return nil, sdk.ErrNotSupported
	}
	vers, err := p.uvDownloads(ctx, "", false)
	if err != nil {
		return nil, err
	}
	var out []sdk.AvailableVersion
	latestMinor := -1
	for _, v := range vers {
		if pv, ok := sdk.ParseVersion(v); ok && pv.Minor() > latestMinor {
			latestMinor = pv.Minor()
		}
	}
	for _, v := range vers {
		pv, _ := sdk.ParseVersion(v)
		out = append(out, sdk.AvailableVersion{
			Version:     v,
			Label:       "Python " + v,
			Vendor:      "CPython",
			Recommended: pv.Minor() == latestMinor-1, // 最新次版本的前一个：第三方轮子最齐
		})
	}
	return &sdk.AvailableToolchainsResult{Versions: out}, nil
}

type uvPythonEntry struct {
	Key            string `json:"key"`
	Version        string `json:"version"`
	Path           string `json:"path"`
	Variant        string `json:"variant"`
	Implementation string `json:"implementation"`
}

// uvDownloads 用 `uv python list --only-downloads` 拿可下载的稳定版 CPython 版本（降序）。
func (p *Plugin) uvDownloads(ctx *sdk.Context, console string, all bool) ([]string, error) {
	uv, err := p.ensureUV(ctx, console)
	if err != nil {
		return nil, err
	}
	args := []string{"python", "list", "--only-downloads", "--output-format", "json"}
	if all {
		args = append(args, "--all-versions")
	}
	r, err := ctx.Host.ExecCapture(ctx, "", nil, uv, args...)
	if err != nil {
		return nil, err
	}
	if r.ExitCode != 0 {
		return nil, sdk.ExecError("uv python list", r)
	}
	var entries []uvPythonEntry
	if err := json.Unmarshal([]byte(r.Stdout), &entries); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		if e.Implementation != "cpython" || (e.Variant != "" && e.Variant != "default") {
			continue
		}
		v, ok := sdk.ParseVersion(e.Version)
		if !ok || v.Pre != "" || seen[e.Version] {
			continue
		}
		seen[e.Version] = true
		out = append(out, e.Version)
	}
	sdk.SortVersionsDesc(out)
	return out, nil
}

// InstallToolchain 用 uv 把 Python 装到 Dest，直接返回工具链。
func (p *Plugin) InstallToolchain(ctx *sdk.Context, in *sdk.InstallToolchainParams) (*sdk.InstallToolchainResult, error) {
	if in.Kind != "" && in.Kind != "python" {
		return nil, sdk.ErrNotSupported
	}
	uv, err := p.ensureUV(ctx, in.TaskID)
	if err != nil {
		return nil, err
	}
	version := in.Version
	// 约束（如 ">=3.12,<3.13"、"3.12.x"）先解析成具体版本
	if _, ok := sdk.ParseVersion(version); !ok || strings.ContainsAny(version, "<>=^~*x, ") {
		all, err := p.uvDownloads(ctx, in.TaskID, true)
		if err != nil {
			return nil, err
		}
		idx := sdk.BestVersion(all, version, "")
		if idx < 0 {
			return nil, sdk.UserError("%s", ctx.T("err.noPythonVersion", in.Version))
		}
		version = all[idx]
	}
	dest := in.Dest
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, err
	}
	env := map[string]string{"UV_PYTHON_INSTALL_DIR": dest}
	if m := p.mirrors(ctx); m.PythonDist != "" {
		env["UV_PYTHON_INSTALL_MIRROR"] = m.PythonDist
	}
	args := []string{"python", "install", version, "--install-dir", dest, "--no-bin"}
	if runtime.GOOS == "windows" {
		args = append(args, "--no-registry")
	}
	r, err := ctx.Host.Exec(ctx, &sdk.ExecParams{Command: uv, Args: args, Env: env, Console: in.TaskID, Title: ctx.T("step.installPython", version)})
	if err != nil {
		return nil, err
	}
	if r.ExitCode != 0 {
		return nil, sdk.ExecError("uv python install", r)
	}
	res, err := p.DiscoverToolchains(ctx, &sdk.DiscoverToolchainsParams{Kind: "python", Paths: []string{dest}})
	if err != nil {
		return nil, err
	}
	for _, tc := range res.Toolchains {
		if strings.HasPrefix(tc.Version, version) || tc.Version == version {
			return &sdk.InstallToolchainResult{Toolchain: &tc}, nil
		}
	}
	if len(res.Toolchains) > 0 {
		return &sdk.InstallToolchainResult{Toolchain: &res.Toolchains[0]}, nil
	}
	return nil, sdk.UserError("%s", ctx.T("err.installedNotFound", dest))
}

// mirrors 返回初始化时下发的镜像配置。
func (p *Plugin) mirrors(ctx *sdk.Context) sdk.Mirrors {
	if ctx != nil && ctx.Init != nil {
		return ctx.Init.Mirrors
	}
	return sdk.Mirrors{}
}

// findToolchain 在内核已知的 python 工具链里按约束挑一个（运行时没有选定工具链时兜底）。
func (p *Plugin) findToolchain(ctx *sdk.Context, constraint, preferred string) *sdk.Toolchain {
	var tcs []sdk.Toolchain
	if list, err := ctx.Host.Toolchains(ctx, "python"); err == nil {
		tcs = list
	}
	if len(tcs) == 0 {
		if res, err := p.DiscoverToolchains(ctx, &sdk.DiscoverToolchainsParams{Kind: "python"}); err == nil {
			tcs = res.Toolchains
		}
	}
	vs := make([]string, len(tcs))
	for i, t := range tcs {
		vs[i] = t.Version
	}
	if idx := sdk.BestVersion(vs, constraint, preferred); idx >= 0 {
		return &tcs[idx]
	}
	return nil
}
