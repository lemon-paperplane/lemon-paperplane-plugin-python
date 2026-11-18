package python

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// Model 是 python 插件给内核与依赖插件（django / fastapi / flask）的模型。
type Model struct {
	Manager        string   `json:"manager"`                  // uv | poetry | pdm | pipenv | conda | pip | none
	LockFile       string   `json:"lockFile,omitempty"`       // 锁文件（相对模块目录）
	DepFiles       []string `json:"depFiles,omitempty"`       // 安装依赖用到的声明文件
	Dependencies   []string `json:"dependencies,omitempty"`   // 直接依赖（规范化包名）
	RequiresPython string   `json:"requiresPython,omitempty"` // 版本约束
	PythonPin      string   `json:"pythonPin,omitempty"`      // .python-version / runtime.txt
	Venv           string   `json:"venv,omitempty"`           // 正在用的虚拟环境（绝对路径）
	VenvPython     string   `json:"venvPython,omitempty"`     // 虚拟环境解释器
	VenvVersion    string   `json:"venvVersion,omitempty"`
	VenvBroken     bool     `json:"venvBroken,omitempty"`
	VenvTarget     string   `json:"venvTarget,omitempty"` // 没有虚拟环境时将在这里创建
	SourceRoots    []string `json:"sourceRoots,omitempty"`
	ProjectName    string   `json:"projectName,omitempty"`
	Entries        int      `json:"entries"` // 生成的 python-script 运行配置数（框架插件据此决定是否接管默认运行项）
}

// analysis 是对一个模块目录的完整静态分析结果（插件内部用）。
type analysis struct {
	dir     string
	pf      *projectFiles
	manager string
	req     pythonReq
	venv    *venvInfo
	venvs   []*venvInfo
}

// analyzeDir 静态分析一个目录（只读文件）。
func analyzeDir(dir string) *analysis {
	pf := readProject(dir, nil)
	a := &analysis{dir: dir, pf: pf, manager: pf.manager(), req: pf.pythonRequirement()}
	a.venvs = findProjectVenvs(dir)
	switch a.manager {
	case mgrConda:
		name, prefix := envYmlInfo(filepath.Join(dir, pf.EnvYml))
		if prefix != "" {
			if v := inspectConda(sdk.Expand(prefix)); v != nil {
				a.venvs = append([]*venvInfo{v}, a.venvs...)
			}
		} else if v := condaEnvByName(name); v != nil {
			a.venvs = append([]*venvInfo{v}, a.venvs...)
		}
	case mgrPoetry:
		if len(a.venvs) == 0 {
			a.venvs = poetryVenvCandidates(dir, pf.name())
		}
	}
	a.venv = pickVenv(a.venvs, a.req.Constraint)
	return a
}

// depFiles 返回参与安装的声明文件（相对模块目录）。
func (a *analysis) depFiles() []string {
	var out []string
	pf := a.pf
	if lf := pf.lockFile(); lf != "" {
		out = append(out, lf)
	}
	switch a.manager {
	case mgrConda:
		out = append(out, pf.EnvYml)
	case mgrUV, mgrPoetry, mgrPDM:
		out = append(out, "pyproject.toml")
	case mgrPipenv:
		out = append(out, "Pipfile")
	case mgrPip:
		out = append(out, a.installFiles()...)
	}
	return out
}

// installFiles 是 pip 方式安装时传给 `uv pip install -r` 的文件。
func (a *analysis) installFiles() []string {
	pf := a.pf
	if len(pf.Requirements) > 0 {
		return pf.Requirements[:1]
	}
	if pf.Pyproject != nil && pf.Pyproject.Project != nil && len(pf.Pyproject.Project.Dependencies) > 0 {
		return []string{"pyproject.toml"}
	}
	if pf.SetupCfg {
		return []string{"setup.cfg"}
	}
	if pf.SetupPy {
		return []string{"setup.py"}
	}
	if pf.Pyproject != nil && (a.manager == mgrUV || a.manager == mgrPDM) {
		return []string{"pyproject.toml"}
	}
	return nil
}

// sourceRoots：模块目录本身，以及 src 布局的 src/（PyCharm 默认把内容根与源码根加进 PYTHONPATH）。
func (a *analysis) sourceRoots() []string {
	roots := []string{a.dir}
	src := filepath.Join(a.dir, "src")
	if sdk.DirExists(src) {
		ents, _ := os.ReadDir(src)
		for _, e := range ents {
			if (e.IsDir() && sdk.FileExists(filepath.Join(src, e.Name(), "__init__.py"))) || strings.HasSuffix(e.Name(), ".py") {
				roots = append(roots, src)
				break
			}
		}
	}
	return roots
}

func (a *analysis) model() *Model {
	m := &Model{
		Manager:        a.manager,
		LockFile:       a.pf.lockFile(),
		DepFiles:       a.depFiles(),
		RequiresPython: a.req.Constraint,
		PythonPin:      a.req.Preferred,
		SourceRoots:    a.sourceRoots(),
		ProjectName:    a.pf.name(),
	}
	for _, r := range a.pf.declaredDeps() {
		if r.Name != "" {
			m.Dependencies = append(m.Dependencies, r.Name)
		}
	}
	if a.venv != nil {
		m.Venv = a.venv.Dir
		m.VenvPython = a.venv.Python
		m.VenvVersion = a.venv.Version
		m.VenvBroken = a.venv.Broken != ""
	} else if a.manager != mgrConda {
		m.VenvTarget = filepath.Join(a.dir, ".venv")
	}
	return m
}

// Analyze 静态分析模块。
func (p *Plugin) Analyze(ctx *sdk.Context, in *sdk.AnalyzeParams) (*sdk.AnalyzeResult, error) {
	a := analyzeDir(in.Module.Dir)
	res := &sdk.AnalyzeResult{}
	m := a.model()

	// 工具链需求
	req := sdk.ToolchainRequirement{Kind: "python", Constraint: a.req.Constraint, Preferred: a.req.Preferred, Source: a.req.Source}
	if req.Preferred == "" && a.venv != nil && a.venv.Broken == "" && a.venv.Version != "" {
		req.Preferred = minorRange(a.venv.Version)
		if req.Source == "" {
			req.Source = "pyvenv.cfg"
		}
	}
	if req.Preferred == "" && req.Constraint == "" {
		// 没有任何版本线索：偏好 3.12（第三方二进制轮子最齐），没有则由内核取最高版本
		req.Preferred = "3.12"
	}
	res.Requirements = []sdk.ToolchainRequirement{req}

	// 依赖状态
	st := p.depState(ctx, a)
	res.Dependencies = &st

	// 问题
	res.Problems = p.problems(ctx, a)

	// 关键信息
	pyVer := ""
	if a.venv != nil && a.venv.Version != "" {
		pyVer = a.venv.Version
	} else if a.req.Preferred != "" {
		pyVer = a.req.Preferred
	} else if a.req.Constraint != "" {
		pyVer = a.req.Constraint
	}
	if pyVer != "" {
		res.Info = append(res.Info, sdk.InfoItem{Label: "Python", Value: pyVer, Icon: "icons/python.svg"})
	}
	if a.manager != mgrNone {
		res.Info = append(res.Info, sdk.InfoItem{Label: ctx.T("info.manager"), Value: managerLabel(a.manager), Icon: managerIcon(a.manager)})
	}
	if a.venv != nil {
		res.Info = append(res.Info, sdk.InfoItem{Label: ctx.T("info.venv"), Value: venvLabel(a)})
	}

	// 运行配置
	res.RunConfigurations = p.runConfigs(ctx, in, a)
	m.Entries = len(res.RunConfigurations)
	res.Model, _ = json.Marshal(m)
	return res, nil
}

func managerLabel(m string) string {
	switch m {
	case mgrUV:
		return "uv"
	case mgrPoetry:
		return "Poetry"
	case mgrPDM:
		return "PDM"
	case mgrPipenv:
		return "Pipenv"
	case mgrConda:
		return "conda"
	case mgrPip:
		return "pip"
	}
	return m
}

func managerIcon(m string) string {
	switch m {
	case mgrUV:
		return "icons/uv.svg"
	case mgrPoetry:
		return "icons/poetry.svg"
	case mgrPDM:
		return "icons/pdm.svg"
	case mgrConda:
		return "icons/anaconda.svg"
	}
	return "builtin:package"
}

func venvLabel(a *analysis) string {
	if a.venv.Kind == "conda" {
		if r, err := filepath.Rel(a.dir, a.venv.Dir); err == nil && !strings.HasPrefix(r, "..") {
			return r
		}
		return "conda: " + filepath.Base(a.venv.Dir)
	}
	return rel(a.dir, a.venv.Dir)
}

// rel 返回相对路径，不在目录内时返回原路径。
func rel(base, path string) string {
	if r, err := filepath.Rel(base, path); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return path
}

// problems 是静态就能发现的问题。
func (p *Plugin) problems(ctx *sdk.Context, a *analysis) []sdk.Diagnosis {
	var out []sdk.Diagnosis
	if a.pf.PyprojectErr != nil {
		out = append(out, sdk.Diagnosis{RuleID: "python.pyproject-invalid", Severity: "error", Title: ctx.T("diag.pyprojectInvalid.title"), Message: a.pf.PyprojectErr.Error()})
	}
	if a.venv != nil && a.venv.Broken != "" && a.venv.Kind == "project" {
		out = append(out, sdk.Diagnosis{
			RuleID: "python.venv-broken", Severity: "error",
			Title:   ctx.T("diag.venvBroken.title", rel(a.dir, a.venv.Dir)),
			Message: ctx.T("diag.venvBroken.message", a.venv.Home),
			Fixes:   []sdk.Fix{recreateVenvFix(ctx, rel(a.dir, a.venv.Dir))},
		})
	} else if a.venv != nil && a.venv.Kind == "project" && a.req.Constraint != "" && a.venv.Version != "" && !sdk.MatchVersion(a.req.Constraint, a.venv.Version) {
		out = append(out, sdk.Diagnosis{
			RuleID: "python.venv-version", Severity: "warning",
			Title:   ctx.T("diag.venvVersion.title", a.venv.Version, a.req.Constraint),
			Message: ctx.T("diag.venvVersion.message", a.req.Source),
			Fixes:   []sdk.Fix{recreateVenvFix(ctx, rel(a.dir, a.venv.Dir))},
		})
	}
	if a.manager == mgrConda && condaExecutable() == "" {
		out = append(out, sdk.Diagnosis{RuleID: "python.conda-missing", Severity: "warning", Title: ctx.T("err.condaMissing"),
			Fixes: []sdk.Fix{{ID: "conda", Title: ctx.T("fix.downloadConda"), Kind: sdk.FixOpenURL, Params: map[string]string{"url": "https://conda-forge.org/download/"}}}})
	}
	return out
}

func recreateVenvFix(ctx *sdk.Context, name string) sdk.Fix {
	return sdk.Fix{ID: "recreateVenv", Title: ctx.T("fix.recreateVenv"), Kind: sdk.FixCustom, Recommended: true, Confirm: ctx.T("confirm.recreateVenv", name)}
}

// ---------------------------------------------------------------------------
// 运行配置候选

var (
	mainGuardRe = regexp.MustCompile(`(?m)^if\s+__name__\s*==\s*['"]__main__['"]\s*:`)
	identRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// 常见入口文件名，按优先级。
var commonEntries = []string{"main.py", "app.py", "run.py", "server.py", "start.py", "bot.py", "cli.py", "__main__.py"}

// 不应作为运行入口的脚本。
var skipScripts = map[string]bool{"setup.py": true, "conftest.py": true, "manage.py": true, "noxfile.py": true, "fabfile.py": true, "tasks.py": true, "wsgi.py": true, "asgi.py": true, "settings.py": true, "__init__.py": true, "config.py": true}

// mainScripts 列出目录（只看这一层）里带 __main__ 入口的脚本，最多 limit 个。
func mainScripts(dir string, files []string, limit int) []string {
	var out []string
	n := 0
	for _, f := range files {
		if !strings.HasSuffix(f, ".py") || skipScripts[f] || strings.HasPrefix(f, "test_") || strings.HasSuffix(f, "_test.py") {
			continue
		}
		n++
		if n > 60 {
			break
		}
		if mainGuardRe.MatchString(sdk.ReadText(filepath.Join(dir, f), 512<<10)) {
			out = append(out, f)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

// frameworkScripts 汇总框架插件在 facet 数据里声明「我来负责运行」的脚本，python 不再重复生成。
func frameworkScripts(facets []sdk.Facet) (map[string]bool, bool) {
	out := map[string]bool{}
	framework := false
	for _, f := range facets {
		if f.Data == nil {
			continue
		}
		if c, _ := f.Data["category"].(string); c == "framework" {
			framework = true
		}
		if list, ok := f.Data["scripts"].([]any); ok {
			for _, s := range list {
				if str, ok := s.(string); ok {
					out[filepath.ToSlash(str)] = true
				}
			}
		}
	}
	return out, framework
}

func (p *Plugin) runConfigs(ctx *sdk.Context, in *sdk.AnalyzeParams, a *analysis) []sdk.RunConfiguration {
	var out []sdk.RunConfiguration
	claimed, framework := frameworkScripts(in.Module.Facets)
	add := func(name string, opts map[string]any, prio int) {
		id := sdk.StableID("python-script", sdk.OptString(opts, "script", ""), sdk.OptString(opts, "module", ""))
		for _, c := range out {
			if c.ID == id {
				return
			}
		}
		out = append(out, sdk.RunConfiguration{ID: id, Name: name, Type: "python-script", Options: opts, Priority: prio, Source: "auto"})
	}

	// 1. IDE 里已有的运行配置（PyCharm / VS Code）
	for _, c := range ideConfigs(in) {
		out = append(out, c)
	}

	// 2. [project.scripts] / [tool.poetry.scripts]
	for _, e := range a.entryPoints() {
		add(e.name, map[string]any{"module": e.target}, 40)
	}

	// 3. 根目录的常见入口与带 __main__ 的脚本
	files := listDir(a.dir)
	has := map[string]bool{}
	for _, f := range files {
		has[f] = true
	}
	mains := mainScripts(a.dir, files, 8)
	isMain := map[string]bool{}
	for _, f := range mains {
		isMain[f] = true
	}
	for i, f := range commonEntries {
		// 没有 __main__ 的 main.py / app.py 也可能是脚本；但有 Web 框架时它多半是应用定义文件，交给框架插件
		if has[f] && !claimed[f] && (isMain[f] || (!framework && (f == "main.py" || f == "app.py"))) {
			add(strings.TrimSuffix(f, ".py"), map[string]any{"script": f}, 35-i)
		}
	}
	for _, f := range mains {
		if !claimed[f] {
			add(strings.TrimSuffix(f, ".py"), map[string]any{"script": f}, 20)
		}
	}

	// 4. 包里的 __main__.py（python -m pkg）
	for _, mod := range mainModules(a) {
		add(mod, map[string]any{"module": mod}, 15)
	}

	if len(out) > 0 && !framework {
		best := 0
		for i, c := range out {
			if c.Priority > out[best].Priority {
				best = i
			}
		}
		out[best].Default = true
	}
	return out
}

type entryPoint struct{ name, target string }

// entryPoints 读 [project.scripts]、[project.gui-scripts] 与 [tool.poetry.scripts]。
func (a *analysis) entryPoints() []entryPoint {
	pp := a.pf.Pyproject
	if pp == nil {
		return nil
	}
	m := map[string]string{}
	if pp.Project != nil {
		for k, v := range pp.Project.Scripts {
			m[k] = v
		}
		for k, v := range pp.Project.GUIScripts {
			m[k] = v
		}
	}
	if pp.Tool.Poetry != nil {
		for k, v := range pp.Tool.Poetry.Scripts {
			switch t := v.(type) {
			case string:
				m[k] = t
			case map[string]any:
				if r, ok := t["callable"].(string); ok {
					m[k] = r
				} else if r, ok := t["reference"].(string); ok && t["type"] != "file" {
					m[k] = r
				}
			}
		}
	}
	var out []entryPoint
	for k, v := range m {
		if strings.Contains(v, ":") {
			out = append(out, entryPoint{k, strings.TrimSpace(strings.SplitN(v, "[", 2)[0])})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

// mainModules 在源码根下（最多 3 层）找带 __main__.py 的包。
func mainModules(a *analysis) []string {
	var out []string
	for _, root := range a.sourceRoots() {
		var walk func(dir, mod string, depth int)
		walk = func(dir, mod string, depth int) {
			if depth > 3 || len(out) >= 5 {
				return
			}
			ents, _ := os.ReadDir(dir)
			for _, e := range ents {
				if !e.IsDir() || !identRe.MatchString(e.Name()) || sdk.DefaultSkipDirs[e.Name()] || e.Name() == "tests" || e.Name() == "test" {
					continue
				}
				sub := filepath.Join(dir, e.Name())
				if !sdk.FileExists(filepath.Join(sub, "__init__.py")) && !sdk.FileExists(filepath.Join(sub, "__main__.py")) {
					continue
				}
				name := e.Name()
				if mod != "" {
					name = mod + "." + e.Name()
				}
				if sdk.FileExists(filepath.Join(sub, "__main__.py")) {
					out = append(out, name)
				}
				walk(sub, name, depth+1)
			}
		}
		walk(root, "", 1)
	}
	return out
}
