package python

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// 包管理器 id（同时也是 facet id，pip 除外）。
const (
	mgrUV     = "uv"
	mgrPoetry = "poetry"
	mgrPDM    = "pdm"
	mgrPipenv = "pipenv"
	mgrConda  = "conda"
	mgrPip    = "pip"
	mgrNone   = "none"
)

// pyproject 只取用到的字段。
type pyproject struct {
	Project *struct {
		Name           string              `toml:"name"`
		RequiresPython string              `toml:"requires-python"`
		Dependencies   []string            `toml:"dependencies"`
		Optional       map[string][]string `toml:"optional-dependencies"`
		Scripts        map[string]string   `toml:"scripts"`
		GUIScripts     map[string]string   `toml:"gui-scripts"`
	} `toml:"project"`
	DependencyGroups map[string][]any `toml:"dependency-groups"`
	BuildSystem      *struct {
		Requires []string `toml:"requires"`
		Backend  string   `toml:"build-backend"`
	} `toml:"build-system"`
	Tool struct {
		Poetry *poetrySection `toml:"poetry"`
		UV     map[string]any `toml:"uv"`
		PDM    map[string]any `toml:"pdm"`
	} `toml:"tool"`
}

type poetrySection struct {
	Name            string         `toml:"name"`
	Dependencies    map[string]any `toml:"dependencies"`
	DevDependencies map[string]any `toml:"dev-dependencies"`
	Group           map[string]struct {
		Dependencies map[string]any `toml:"dependencies"`
	} `toml:"group"`
	Scripts map[string]any `toml:"scripts"`
}

// projectFiles 是一个目录里与 Python 相关的文件及其解析结果。
type projectFiles struct {
	Dir   string
	Files []string // 目录里的名字，子目录以 / 结尾

	Pyproject    *pyproject
	PyprojectErr error
	Requirements []string // requirements*.txt（相对 Dir），按优先级
	SetupPy      bool
	SetupCfg     bool
	Pipfile      bool
	PipfileLock  bool
	EnvYml       string // environment.yml / environment.yaml
	UVLock       bool
	PoetryLock   bool
	PDMLock      bool
	ManagePy     bool
}

// readProject 读取目录。files 为空时自己列目录。
func readProject(dir string, files []string) *projectFiles {
	if files == nil {
		files = listDir(dir)
	}
	pf := &projectFiles{Dir: dir, Files: files}
	has := map[string]bool{}
	for _, f := range files {
		has[f] = true
	}
	pf.SetupPy = has["setup.py"]
	pf.SetupCfg = has["setup.cfg"]
	pf.Pipfile = has["Pipfile"]
	pf.PipfileLock = has["Pipfile.lock"]
	pf.UVLock = has["uv.lock"]
	pf.PoetryLock = has["poetry.lock"]
	pf.PDMLock = has["pdm.lock"]
	pf.ManagePy = has["manage.py"]
	for _, n := range []string{"environment.yml", "environment.yaml"} {
		if has[n] {
			pf.EnvYml = n
			break
		}
	}
	pf.Requirements = requirementFiles(dir, files)
	if has["pyproject.toml"] {
		var pp pyproject
		if _, err := toml.DecodeFile(filepath.Join(dir, "pyproject.toml"), &pp); err != nil {
			pf.PyprojectErr = err
		} else {
			pf.Pyproject = &pp
		}
	}
	return pf
}

func listDir(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name()+"/")
		} else {
			out = append(out, e.Name())
		}
	}
	return out
}

// requirementFiles 按优先级列出要安装的 requirements 文件：
// requirements.txt 优先；其次 requirements/ 目录下的 base/common/prod 等；最后其他 requirements-*.txt。
func requirementFiles(dir string, files []string) []string {
	var main, others []string
	for _, f := range files {
		if f == "requirements.txt" {
			main = append(main, f)
		} else if strings.HasPrefix(f, "requirements") && strings.HasSuffix(f, ".txt") && !strings.HasSuffix(f, "/") {
			others = append(others, f)
		}
	}
	if len(main) > 0 {
		return append(main, others...)
	}
	for _, f := range files {
		if f == "requirements/" {
			sub := listDir(filepath.Join(dir, "requirements"))
			for _, pref := range []string{"base.txt", "common.txt", "main.txt", "prod.txt", "production.txt", "requirements.txt", "local.txt", "dev.txt", "development.txt"} {
				for _, s := range sub {
					if s == pref {
						main = append(main, "requirements/"+s)
					}
				}
			}
		}
	}
	// 其他 requirements-xxx.txt：生产/基础优先，开发与测试靠后
	sort.SliceStable(others, func(i, j int) bool { return reqRank(others[i]) < reqRank(others[j]) })
	return append(main, others...)
}

func reqRank(name string) int {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "prod") || strings.Contains(n, "base") || strings.Contains(n, "main") || strings.Contains(n, "common"):
		return 0
	case strings.Contains(n, "dev") || strings.Contains(n, "test") || strings.Contains(n, "doc") || strings.Contains(n, "lint"):
		return 2
	}
	return 1
}

// isPythonPyproject：pyproject 里真的有 Python 工程声明（而不只是 black/ruff 等工具配置）。
func (pf *projectFiles) isPythonPyproject() bool {
	pp := pf.Pyproject
	if pp == nil {
		return pf.PyprojectErr != nil // 解析失败也当作 Python 工程，后面报问题
	}
	return pp.Project != nil || pp.Tool.Poetry != nil || pp.BuildSystem != nil || pp.Tool.PDM != nil || pp.Tool.UV != nil
}

// strongMarker：带依赖声明或锁文件的强特征。
func (pf *projectFiles) strongMarker() bool {
	return pf.isPythonPyproject() || pf.SetupPy || pf.SetupCfg || pf.Pipfile || pf.EnvYml != "" || pf.UVLock || pf.PoetryLock || pf.PDMLock
}

// hasPyFiles：目录里有 .py，或有 Python 包（带 __init__.py 的子目录），或 src/ 下有 Python 代码。
func (pf *projectFiles) hasPyFiles() bool {
	n := 0
	for _, f := range pf.Files {
		if strings.HasSuffix(f, ".py") {
			return true
		}
		if strings.HasSuffix(f, "/") && n < 30 {
			n++
			sub := filepath.Join(pf.Dir, strings.TrimSuffix(f, "/"))
			if sdk.FileExists(filepath.Join(sub, "__init__.py")) {
				return true
			}
			if f == "src/" {
				for _, e := range listDir(sub) {
					if strings.HasSuffix(e, ".py") || (strings.HasSuffix(e, "/") && sdk.FileExists(filepath.Join(sub, e, "__init__.py"))) {
						return true
					}
				}
			}
		}
	}
	return false
}

// manager 推断包管理器。
func (pf *projectFiles) manager() string {
	pp := pf.Pyproject
	switch {
	case pf.EnvYml != "":
		return mgrConda
	case pf.UVLock:
		return mgrUV
	case pf.PoetryLock:
		return mgrPoetry
	case pf.PDMLock:
		return mgrPDM
	case pf.Pipfile:
		return mgrPipenv
	case pp != nil && pp.Tool.Poetry != nil && len(pp.Tool.Poetry.Dependencies) > 0:
		return mgrPoetry
	case pp != nil && pp.Tool.PDM != nil:
		return mgrPDM
	case pp != nil && pp.Tool.UV != nil:
		return mgrUV
	case len(pf.Requirements) > 0, pp != nil && pp.Project != nil, pf.SetupPy, pf.SetupCfg:
		return mgrPip
	}
	return mgrNone
}

// lockFile 返回锁文件名（相对 Dir）。
func (pf *projectFiles) lockFile() string {
	switch pf.manager() {
	case mgrUV:
		if pf.UVLock {
			return "uv.lock"
		}
	case mgrPoetry:
		if pf.PoetryLock {
			return "poetry.lock"
		}
	case mgrPDM:
		if pf.PDMLock {
			return "pdm.lock"
		}
	case mgrPipenv:
		if pf.PipfileLock {
			return "Pipfile.lock"
		}
	}
	return ""
}

// name 返回建议的模块名。
func (pf *projectFiles) name() string {
	if pp := pf.Pyproject; pp != nil {
		if pp.Project != nil && pp.Project.Name != "" {
			return pp.Project.Name
		}
		if pp.Tool.Poetry != nil && pp.Tool.Poetry.Name != "" {
			return pp.Tool.Poetry.Name
		}
	}
	if pf.SetupCfg {
		if n := iniValue(filepath.Join(pf.Dir, "setup.cfg"), "metadata", "name"); n != "" {
			return n
		}
	}
	if pf.SetupPy {
		if m := setupNameRe.FindStringSubmatch(sdk.ReadText(filepath.Join(pf.Dir, "setup.py"), 256<<10)); m != nil {
			return m[1]
		}
	}
	return ""
}

var (
	setupNameRe     = regexp.MustCompile(`\bname\s*=\s*["']([^"']+)["']`)
	setupPyReqRe    = regexp.MustCompile(`(?s)install_requires\s*=\s*\[(.*?)\]`)
	setupPyPyReqRe  = regexp.MustCompile(`python_requires\s*=\s*["']([^"']+)["']`)
	quotedStringRe  = regexp.MustCompile(`["']([^"']+)["']`)
	pyVersionNumRe  = regexp.MustCompile(`(\d+\.\d+(?:\.\d+)?)`)
	condaPythonRe   = regexp.MustCompile(`^\s*-\s*python\s*([=<>!~]=?)?\s*([\d.*]+)?`)
	envYmlNameRe    = regexp.MustCompile(`(?m)^name:\s*["']?([^"'\s#]+)`)
	envYmlPrefixRe  = regexp.MustCompile(`(?m)^prefix:\s*["']?([^"'#\n]+)`)
	pipfilePyRe     = regexp.MustCompile(`python_(?:full_)?version\s*=\s*["']([^"']+)["']`)
	runtimeTxtRe    = regexp.MustCompile(`python-(\d+\.\d+(?:\.\d+)?)`)
	pythonVersionRe = regexp.MustCompile(`^(?:cpython-|python)?(\d+\.\d+(?:\.\d+)?)`)
)

// declaredDeps 返回直接依赖（用于 facet 数据与「是否已安装」检查）。只取主依赖，不含可选与开发组。
func (pf *projectFiles) declaredDeps() []requirement {
	pp := pf.Pyproject
	switch pf.manager() {
	case mgrConda:
		return envYmlPipDeps(filepath.Join(pf.Dir, pf.EnvYml))
	case mgrPoetry:
		if pp != nil && pp.Tool.Poetry != nil && len(pp.Tool.Poetry.Dependencies) > 0 {
			return poetryRequirements(pp.Tool.Poetry.Dependencies)
		}
	case mgrPipenv:
		return pipfileRequirements(filepath.Join(pf.Dir, "Pipfile"), false)
	case mgrUV, mgrPDM:
		if pp != nil && pp.Project != nil {
			return parseReqList(pp.Project.Dependencies)
		}
	}
	if len(pf.Requirements) > 0 {
		return parseRequirementsFile(filepath.Join(pf.Dir, pf.Requirements[0]))
	}
	if pp != nil && pp.Project != nil {
		return parseReqList(pp.Project.Dependencies)
	}
	if pf.SetupCfg {
		if v := iniValue(filepath.Join(pf.Dir, "setup.cfg"), "options", "install_requires"); v != "" {
			return parseReqList(strings.Split(v, "\n"))
		}
	}
	if pf.SetupPy {
		text := sdk.ReadText(filepath.Join(pf.Dir, "setup.py"), 256<<10)
		if m := setupPyReqRe.FindStringSubmatch(text); m != nil {
			var list []string
			for _, q := range quotedStringRe.FindAllStringSubmatch(m[1], -1) {
				list = append(list, q[1])
			}
			return parseReqList(list)
		}
	}
	return nil
}

// hasDeclarations 表示工程声明了依赖（需要虚拟环境）。
func (pf *projectFiles) hasDeclarations() bool {
	if pf.manager() == mgrNone {
		return false
	}
	if pf.manager() == mgrConda || pf.lockFile() != "" || len(pf.Requirements) > 0 {
		return true
	}
	return len(pf.declaredDeps()) > 0
}

func parseReqList(list []string) []requirement {
	var out []requirement
	for _, s := range list {
		if r, ok := parseRequirement(s); ok {
			out = append(out, r)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Python 版本要求

type pythonReq struct {
	Constraint string // PEP 440 / npm 风格约束
	Preferred  string
	Source     string
	PinSource  string
}

// pythonRequirement 汇总工程对 Python 版本的要求。
func (pf *projectFiles) pythonRequirement() pythonReq {
	var r pythonReq
	if pp := pf.Pyproject; pp != nil {
		if pp.Project != nil && pp.Project.RequiresPython != "" {
			r.Constraint, r.Source = cleanPyConstraint(pp.Project.RequiresPython), "pyproject.toml"
		} else if pp.Tool.Poetry != nil {
			if v, ok := pp.Tool.Poetry.Dependencies["python"].(string); ok && v != "" && v != "*" {
				r.Constraint, r.Source = poetryConstraint(v), "pyproject.toml"
			}
		}
	}
	if r.Constraint == "" && pf.Pipfile {
		if m := pipfilePyRe.FindStringSubmatch(sdk.ReadText(filepath.Join(pf.Dir, "Pipfile"), 64<<10)); m != nil {
			r.Constraint, r.Source = minorRange(m[1]), "Pipfile"
		}
	}
	if r.Constraint == "" && pf.EnvYml != "" {
		if c := envYmlPython(filepath.Join(pf.Dir, pf.EnvYml)); c != "" {
			r.Constraint, r.Source = c, pf.EnvYml
		}
	}
	if r.Constraint == "" && pf.SetupCfg {
		if v := iniValue(filepath.Join(pf.Dir, "setup.cfg"), "options", "python_requires"); v != "" {
			r.Constraint, r.Source = cleanPyConstraint(v), "setup.cfg"
		}
	}
	if r.Constraint == "" && pf.SetupPy {
		if m := setupPyPyReqRe.FindStringSubmatch(sdk.ReadText(filepath.Join(pf.Dir, "setup.py"), 256<<10)); m != nil {
			r.Constraint, r.Source = cleanPyConstraint(m[1]), "setup.py"
		}
	}
	// 显式钉住的版本：.python-version（pyenv/uv）、runtime.txt（Heroku）
	if v := readPythonVersionFile(filepath.Join(pf.Dir, ".python-version")); v != "" {
		r.Preferred, r.PinSource = v, ".python-version"
	} else if m := runtimeTxtRe.FindStringSubmatch(sdk.ReadText(filepath.Join(pf.Dir, "runtime.txt"), 4096)); m != nil {
		r.Preferred, r.PinSource = m[1], "runtime.txt"
	}
	if r.Preferred != "" && r.Constraint == "" {
		r.Constraint, r.Source = minorRange(r.Preferred), r.PinSource
	}
	return r
}

// readPythonVersionFile 读 .python-version 第一条有效版本（"3.12"、"3.12.4"、"cpython-3.12"）。
func readPythonVersionFile(path string) string {
	for _, line := range strings.Split(sdk.ReadText(path, 4096), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := pythonVersionRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
		return ""
	}
	return ""
}

// cleanPyConstraint 把 PEP 440 约束整理成 SDK 能解析的形式（去空白、!= 片段丢弃）。
func cleanPyConstraint(s string) string {
	var parts []string
	for _, p := range strings.Split(s, ",") {
		p = strings.ReplaceAll(strings.TrimSpace(p), " ", "")
		if p == "" || strings.HasPrefix(p, "!=") {
			continue
		}
		if strings.HasPrefix(p, "===") {
			p = "=" + strings.TrimPrefix(p, "===")
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ",")
}

// minorRange 把 "3.11" / "3.11.4" 变成同一次版本的范围 "3.11"（x-range）。
func minorRange(v string) string {
	m := pyVersionNumRe.FindStringSubmatch(v)
	if m == nil {
		return ""
	}
	parts := strings.Split(m[1], ".")
	return parts[0] + "." + parts[1]
}

// poetryConstraint 转换 Poetry 版本写法（^3.10、~3.10、>=3.9,<4、3.10.*）。
func poetryConstraint(s string) string {
	var out []string
	for _, alt := range strings.Split(s, "||") {
		alt = strings.TrimSpace(alt)
		out = append(out, cleanPyConstraint(poetryVersion(alt)))
	}
	return strings.Join(out, " || ")
}

// envYmlPython 从 environment.yml 的 dependencies 里找 python=3.10 之类。
func envYmlPython(path string) string {
	for _, line := range strings.Split(sdk.ReadText(path, 256<<10), "\n") {
		m := condaPythonRe.FindStringSubmatch(line)
		if m == nil || m[2] == "" {
			continue
		}
		op, v := m[1], strings.TrimSuffix(m[2], ".*")
		switch op {
		case "", "=", "==":
			if strings.Count(v, ".") >= 2 && op == "==" {
				return v
			}
			return minorRange(v)
		default:
			return op + v
		}
	}
	return ""
}

// envYmlInfo 返回 environment.yml 的 name 与 prefix。
func envYmlInfo(path string) (name, prefix string) {
	text := sdk.ReadText(path, 256<<10)
	if m := envYmlNameRe.FindStringSubmatch(text); m != nil {
		name = m[1]
	}
	if m := envYmlPrefixRe.FindStringSubmatch(text); m != nil {
		prefix = strings.TrimSpace(m[1])
	}
	return
}

// envYmlPipDeps 取 environment.yml 里 pip: 小节的依赖。
func envYmlPipDeps(path string) []requirement {
	var out []requirement
	inPip := false
	pipIndent := 0
	for _, line := range strings.Split(sdk.ReadText(path, 256<<10), "\n") {
		trim := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if strings.HasPrefix(trim, "- pip:") {
			inPip, pipIndent = true, indent
			continue
		}
		if inPip {
			if trim == "" {
				continue
			}
			if indent <= pipIndent || !strings.HasPrefix(trim, "-") {
				inPip = false
				continue
			}
			if r, ok := parseRequirement(stripReqComment(strings.TrimSpace(strings.TrimPrefix(trim, "-")))); ok {
				out = append(out, r)
			}
		}
	}
	return out
}

// iniValue 读取 setup.cfg 某节某键（支持续行）。
func iniValue(path, section, key string) string {
	var cur string
	var val strings.Builder
	inKey := false
	for _, line := range strings.Split(sdk.ReadText(path, 256<<10), "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]") {
			if inKey {
				break
			}
			cur = strings.TrimSpace(trim[1 : len(trim)-1])
			continue
		}
		if cur != section {
			continue
		}
		if inKey {
			if line != "" && (line[0] == ' ' || line[0] == '\t') {
				if trim != "" && !strings.HasPrefix(trim, "#") {
					val.WriteString(trim + "\n")
				}
				continue
			}
			break
		}
		if k, v, ok := strings.Cut(trim, "="); ok && strings.TrimSpace(k) == key {
			inKey = true
			if v = strings.TrimSpace(v); v != "" {
				val.WriteString(v + "\n")
			}
		}
	}
	return strings.TrimSpace(val.String())
}

// ---------------------------------------------------------------------------
// Poetry / Pipfile → requirements

// poetryRequirements 把 [tool.poetry.dependencies] 转成 PEP 508 依赖（不含 python 本身与可选依赖）。
func poetryRequirements(deps map[string]any) []requirement {
	names := make([]string, 0, len(deps))
	for n := range deps {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []requirement
	for _, n := range names {
		if strings.EqualFold(n, "python") {
			continue
		}
		spec, marker, local, skip := poetrySpec(deps[n])
		if skip {
			continue
		}
		line := n + spec
		if marker != "" {
			line += "; " + marker
		}
		out = append(out, requirement{Name: normName(n), Spec: line, Marker: marker, Local: local})
	}
	return out
}

func poetrySpec(v any) (spec, marker string, local, skip bool) {
	switch t := v.(type) {
	case string:
		return poetryVersion(t), "", false, false
	case map[string]any:
		if opt, _ := t["optional"].(bool); opt {
			return "", "", false, true
		}
		if _, ok := t["path"]; ok {
			return "", "", true, false
		}
		if g, ok := t["git"].(string); ok {
			return " @ git+" + g, "", false, false
		}
		if ver, ok := t["version"].(string); ok {
			spec = poetryVersion(ver)
		}
		if ex, ok := t["extras"].([]any); ok && len(ex) > 0 {
			var xs []string
			for _, e := range ex {
				if s, ok := e.(string); ok {
					xs = append(xs, s)
				}
			}
			spec = "[" + strings.Join(xs, ",") + "]" + spec
		}
		if m, ok := t["markers"].(string); ok {
			marker = m
		}
		if py, ok := t["python"].(string); ok && py != "" {
			c := cleanPyConstraint(poetryVersion(py))
			if c != "" && !strings.ContainsAny(c[:1], "<>=!~") {
				c = "==" + c
			}
			pm := "python_version " + strings.Replace(c, ",", " and python_version ", -1)
			if marker != "" {
				marker = "(" + marker + ") and " + pm
			} else {
				marker = pm
			}
		}
		return spec, marker, false, false
	case []any:
		// 多约束依赖：取第一个（按平台区分的写法很少见）
		if len(t) > 0 {
			return poetrySpec(t[0])
		}
	}
	return "", "", false, false
}

// poetryVersion 把 ^1.2 / ~1.2 / 1.2.3 / * 转成 PEP 440。
func poetryVersion(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "*" {
		return ""
	}
	var parts []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		switch {
		case strings.HasPrefix(p, "^"):
			v := strings.TrimPrefix(p, "^")
			parts = append(parts, ">="+v, "<"+caretUpper(v))
		case strings.HasPrefix(p, "~") && !strings.HasPrefix(p, "~="):
			v := strings.TrimPrefix(p, "~")
			parts = append(parts, ">="+v, "<"+tildeUpper(v))
		case p != "" && (p[0] >= '0' && p[0] <= '9'):
			if strings.HasSuffix(p, ".*") {
				parts = append(parts, "=="+p)
			} else {
				parts = append(parts, "=="+p)
			}
		default:
			parts = append(parts, strings.ReplaceAll(p, " ", ""))
		}
	}
	return strings.Join(parts, ",")
}

func caretUpper(v string) string {
	segs := strings.Split(v, ".")
	for i, s := range segs {
		if s != "0" || i == len(segs)-1 {
			n := atoi(s) + 1
			out := append([]string{}, segs[:i]...)
			return strings.Join(append(out, itoa(n)), ".")
		}
	}
	return v
}

func tildeUpper(v string) string {
	segs := strings.Split(v, ".")
	if len(segs) == 1 {
		return itoa(atoi(segs[0]) + 1)
	}
	return segs[0] + "." + itoa(atoi(segs[1])+1)
}

// pipfileRequirements 读 Pipfile 的 [packages]（dev 为 true 时也读 [dev-packages]）。
func pipfileRequirements(path string, dev bool) []requirement {
	var pf struct {
		Packages    map[string]any `toml:"packages"`
		DevPackages map[string]any `toml:"dev-packages"`
	}
	if _, err := toml.DecodeFile(path, &pf); err != nil {
		return nil
	}
	conv := func(m map[string]any) []requirement {
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		var out []requirement
		for _, n := range names {
			line := n
			local := false
			marker := ""
			switch t := m[n].(type) {
			case string:
				if t != "*" {
					line += t
				}
			case map[string]any:
				if _, ok := t["path"]; ok {
					local = true
				}
				if ex, ok := t["extras"].([]any); ok && len(ex) > 0 {
					var xs []string
					for _, e := range ex {
						xs = append(xs, toString(e))
					}
					line += "[" + strings.Join(xs, ",") + "]"
				}
				if v, ok := t["version"].(string); ok && v != "*" {
					line += v
				}
				if g, ok := t["git"].(string); ok {
					line = n + " @ git+" + g
				}
				if mk, ok := t["markers"].(string); ok {
					marker = mk
				}
				if pl, ok := t["sys_platform"].(string); ok {
					marker = joinMarker(marker, "sys_platform "+pl)
				}
			}
			if marker != "" {
				line += "; " + marker
			}
			out = append(out, requirement{Name: normName(n), Spec: line, Marker: marker, Local: local})
		}
		return out
	}
	out := conv(pf.Packages)
	if dev {
		out = append(out, conv(pf.DevPackages)...)
	}
	return out
}

// pipfileLockRequirements 把 Pipfile.lock 转成钉死版本的 requirements（default + develop）。
func pipfileLockRequirements(path string) []requirement {
	var lock map[string]json.RawMessage
	if err := sdk.ReadJSON(path, &lock); err != nil {
		return nil
	}
	var out []requirement
	for _, section := range []string{"default", "develop"} {
		var pkgs map[string]struct {
			Version  string   `json:"version"`
			Markers  string   `json:"markers"`
			Extras   []string `json:"extras"`
			Git      string   `json:"git"`
			Ref      string   `json:"ref"`
			Path     string   `json:"path"`
			Editable bool     `json:"editable"`
		}
		if err := json.Unmarshal(lock[section], &pkgs); err != nil {
			continue
		}
		names := make([]string, 0, len(pkgs))
		for n := range pkgs {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			p := pkgs[n]
			if p.Path != "" {
				continue
			}
			line := n
			if len(p.Extras) > 0 {
				line += "[" + strings.Join(p.Extras, ",") + "]"
			}
			switch {
			case p.Git != "":
				line = n + " @ git+" + p.Git
				if p.Ref != "" {
					line += "@" + p.Ref
				}
			case p.Version != "":
				line += p.Version
			}
			if p.Markers != "" {
				line += "; " + p.Markers
			}
			out = append(out, requirement{Name: normName(n), Spec: line, Marker: p.Markers})
		}
	}
	return out
}

func joinMarker(a, b string) string {
	if a == "" {
		return b
	}
	return "(" + a + ") and " + b
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
