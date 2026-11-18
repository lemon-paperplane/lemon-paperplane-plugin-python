package python

import (
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// venvInfo 描述一个虚拟环境（venv / virtualenv / uv venv）或 conda 环境。
type venvInfo struct {
	Dir     string `json:"dir"`
	Version string `json:"version,omitempty"`
	Home    string `json:"home,omitempty"`
	Python  string `json:"python,omitempty"`
	Broken  string `json:"broken,omitempty"` // 非空表示环境已损坏，值为原因（home 不存在 / python 不存在）
	Conda   bool   `json:"conda,omitempty"`
	Kind    string `json:"kind,omitempty"` // project | poetry | conda
}

// 工程内虚拟环境的常见目录名，按优先级。
var venvDirNames = []string{".venv", "venv", "env", ".env", "virtualenv", ".virtualenv"}

func binDir(prefix string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(prefix, "Scripts")
	}
	return filepath.Join(prefix, "bin")
}

// venvPython 返回虚拟环境里的解释器路径。
func venvPython(dir string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, "Scripts", "python.exe")
	}
	for _, n := range []string{"python", "python3"} {
		p := filepath.Join(dir, "bin", n)
		if _, err := os.Lstat(p); err == nil {
			return p
		}
	}
	return filepath.Join(dir, "bin", "python")
}

// condaPython 返回 conda 环境前缀里的解释器。
func condaPython(prefix string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(prefix, "python.exe")
	}
	return filepath.Join(prefix, "bin", "python")
}

// readPyvenvCfg 读 pyvenv.cfg 的键值。
func readPyvenvCfg(path string) map[string]string {
	text := sdk.ReadText(path, 64<<10)
	if text == "" {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return out
}

// inspectVenv 检查目录是否为虚拟环境；不是则返回 nil。
func inspectVenv(dir string) *venvInfo {
	cfg := readPyvenvCfg(filepath.Join(dir, "pyvenv.cfg"))
	if cfg == nil {
		return nil
	}
	v := &venvInfo{Dir: dir, Home: cfg["home"], Kind: "project"}
	v.Version = cfg["version_info"]
	if v.Version == "" {
		v.Version = cfg["version"]
	}
	if m := pyVersionNumRe.FindStringSubmatch(v.Version); m != nil {
		v.Version = m[1]
	}
	v.Python = venvPython(dir)
	if v.Home != "" && !sdk.DirExists(v.Home) {
		v.Broken = "home"
	} else if !sdk.FileExists(v.Python) { // Stat 会跟随符号链接，悬空链接即不存在
		v.Broken = "python"
	}
	if v.Version == "" {
		v.Version = versionFromLib(dir)
	}
	return v
}

// inspectConda 检查 conda 环境前缀（conda-meta/ 存在）。
func inspectConda(prefix string) *venvInfo {
	if !sdk.DirExists(filepath.Join(prefix, "conda-meta")) {
		return nil
	}
	v := &venvInfo{Dir: prefix, Conda: true, Kind: "conda", Python: condaPython(prefix)}
	v.Version = condaPythonVersion(prefix)
	if !sdk.FileExists(v.Python) {
		v.Broken = "python"
	}
	return v
}

var condaMetaPyRe = regexp.MustCompile(`^python-(\d+\.\d+\.\d+)-`)

// condaPythonVersion 从 conda-meta/python-3.x.y-*.json 文件名取版本。
func condaPythonVersion(prefix string) string {
	ents, _ := os.ReadDir(filepath.Join(prefix, "conda-meta"))
	for _, e := range ents {
		if m := condaMetaPyRe.FindStringSubmatch(e.Name()); m != nil {
			return m[1]
		}
	}
	return ""
}

var libPyRe = regexp.MustCompile(`^python(\d+\.\d+)t?$`)

// versionFromLib 从 lib/python3.X 目录推断次版本号。
func versionFromLib(prefix string) string {
	ents, _ := os.ReadDir(filepath.Join(prefix, "lib"))
	for _, e := range ents {
		if m := libPyRe.FindStringSubmatch(e.Name()); m != nil && e.IsDir() {
			return m[1]
		}
	}
	return ""
}

// findProjectVenvs 列出工程目录里的所有虚拟环境（按优先级）。
func findProjectVenvs(dir string) []*venvInfo {
	var out []*venvInfo
	for _, n := range venvDirNames {
		p := filepath.Join(dir, n)
		if !sdk.DirExists(p) {
			continue
		}
		if v := inspectVenv(p); v != nil {
			out = append(out, v)
		} else if v := inspectConda(p); v != nil {
			out = append(out, v)
		}
	}
	for _, n := range []string{".conda", "conda-env"} {
		if v := inspectConda(filepath.Join(dir, n)); v != nil {
			out = append(out, v)
		}
	}
	return out
}

// pickVenv 在多个虚拟环境中挑一个：有效的优先，其次按目录名优先级；uv 工程优先 .venv。
func pickVenv(vs []*venvInfo, constraint string) *venvInfo {
	if len(vs) == 0 {
		return nil
	}
	best := -1
	score := func(v *venvInfo) int {
		s := 0
		if v.Broken == "" {
			s += 100
		}
		if constraint != "" && v.Version != "" && sdk.MatchVersion(constraint, v.Version) {
			s += 10
		}
		return s
	}
	for i, v := range vs {
		if best < 0 || score(v) > score(vs[best]) {
			best = i
		}
	}
	return vs[best]
}

// poetryVenvDir 计算 Poetry 默认（工程外）虚拟环境目录的前缀，与 poetry 的 generate_env_name 一致。
func poetryVenvCandidates(projectDir, name string) []*venvInfo {
	if name == "" {
		return nil
	}
	var cache string
	switch runtime.GOOS {
	case "darwin":
		cache = filepath.Join(sdk.HomeDir(), "Library", "Caches", "pypoetry", "virtualenvs")
	case "windows":
		cache = filepath.Join(os.Getenv("LOCALAPPDATA"), "pypoetry", "Cache", "virtualenvs")
	default:
		base := os.Getenv("XDG_CACHE_HOME")
		if base == "" {
			base = filepath.Join(sdk.HomeDir(), ".cache")
		}
		cache = filepath.Join(base, "pypoetry", "virtualenvs")
	}
	if env := os.Getenv("POETRY_VIRTUALENVS_PATH"); env != "" {
		cache = env
	}
	real, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		real = projectDir
	}
	prefix := poetryEnvPrefix(name, real)
	ents, _ := os.ReadDir(cache)
	var out []*venvInfo
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), prefix) {
			if v := inspectVenv(filepath.Join(cache, e.Name())); v != nil {
				v.Kind = "poetry"
				out = append(out, v)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return sdk.CompareVersions(out[i].Version, out[j].Version) > 0 })
	return out
}

var poetryUnsafeRe = regexp.MustCompile("[ $`!*@\"\\\\\r\n\t]")

// poetryEnvPrefix 与 poetry 的 EnvManager.generate_env_name 一致：<规范名前 42 字符>-<sha256(路径) 的 urlsafe base64 前 8 位>-py
func poetryEnvPrefix(name, projectDir string) string {
	sanitized := poetryUnsafeRe.ReplaceAllString(strings.ToLower(name), "_")
	if len(sanitized) > 42 {
		sanitized = sanitized[:42]
	}
	if runtime.GOOS == "windows" {
		projectDir = strings.ToLower(projectDir)
	}
	h := sha256.Sum256([]byte(projectDir))
	return sanitized + "-" + base64.URLEncoding.EncodeToString(h[:])[:8] + "-py"
}

// condaEnvByName 在已知 conda 安装里找具名环境。
func condaEnvByName(name string) *venvInfo {
	if name == "" {
		return nil
	}
	for _, root := range condaRoots() {
		if name == "base" {
			if v := inspectConda(root); v != nil {
				return v
			}
			continue
		}
		if v := inspectConda(filepath.Join(root, "envs", name)); v != nil {
			return v
		}
	}
	// ~/.conda/envs
	if v := inspectConda(filepath.Join(sdk.HomeDir(), ".conda", "envs", name)); v != nil {
		return v
	}
	return nil
}

// condaExecutable 找一个 conda / mamba 可执行文件。
func condaExecutable() string {
	for _, root := range condaRoots() {
		cands := []string{filepath.Join(root, "bin", "conda"), filepath.Join(root, "condabin", "conda"), filepath.Join(root, "bin", "mamba")}
		if runtime.GOOS == "windows" {
			cands = []string{filepath.Join(root, "Scripts", "conda.exe"), filepath.Join(root, "condabin", "conda.bat")}
		}
		for _, c := range cands {
			if sdk.FileExists(c) {
				return c
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// site-packages

// sitePackagesDirs 返回环境的 site-packages 目录。
func sitePackagesDirs(prefix string) []string {
	if runtime.GOOS == "windows" {
		p := filepath.Join(prefix, "Lib", "site-packages")
		if sdk.DirExists(p) {
			return []string{p}
		}
		return nil
	}
	var out []string
	ents, _ := os.ReadDir(filepath.Join(prefix, "lib"))
	for _, e := range ents {
		if libPyRe.MatchString(e.Name()) {
			p := filepath.Join(prefix, "lib", e.Name(), "site-packages")
			if sdk.DirExists(p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// installedDist 是 site-packages 里的一个已安装分发包。
type installedDist struct {
	Name    string `json:"name"`    // 显示名（来自目录名）
	Version string `json:"version"` // 版本
	Norm    string `json:"-"`
}

var distInfoRe = regexp.MustCompile(`^(.+?)-([0-9][^-]*?)(?:-py\d.*)?\.(?:dist-info|egg-info)$`)

// installedDists 扫描 site-packages 的 *.dist-info / *.egg-info。
func installedDists(prefix string) map[string]installedDist {
	out := map[string]installedDist{}
	for _, sp := range sitePackagesDirs(prefix) {
		ents, _ := os.ReadDir(sp)
		for _, e := range ents {
			n := e.Name()
			if !strings.HasSuffix(n, ".dist-info") && !strings.HasSuffix(n, ".egg-info") {
				continue
			}
			var d installedDist
			if m := distInfoRe.FindStringSubmatch(n); m != nil {
				d = installedDist{Name: strings.ReplaceAll(m[1], "_", "-"), Version: m[2]}
			} else {
				base := strings.TrimSuffix(strings.TrimSuffix(n, ".dist-info"), ".egg-info")
				d = installedDist{Name: base}
			}
			d.Norm = normName(d.Name)
			out[d.Norm] = d
		}
	}
	return out
}
