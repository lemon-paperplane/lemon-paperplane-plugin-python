package python

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// envTarget 是一次安装/运行要用的 Python 环境。
type envTarget struct {
	Venv   *venvInfo // 工程虚拟环境（或 conda 环境）；nil 表示直接用工具链解释器
	Python string    // 解释器
	Base   *sdk.Toolchain
}

// InstallDependencies 创建虚拟环境（需要时）并按包管理器安装依赖，不改锁文件。
func (p *Plugin) InstallDependencies(ctx *sdk.Context, in *sdk.InstallDepsParams) (*sdk.InstallDepsResult, error) {
	a := analyzeDir(in.Module.Dir)
	var tc *sdk.Toolchain
	if t, ok := in.Toolchains["python"]; ok {
		tc = &t
	}
	if in.Clean && a.venv != nil && a.venv.Kind == "project" {
		ctx.Host.Console(in.TaskID, ctx.T("step.removeVenv", a.venv.Dir))
		if err := os.RemoveAll(a.venv.Dir); err != nil {
			return nil, err
		}
		a = analyzeDir(in.Module.Dir)
	}
	if err := p.installAll(ctx, a, tc, in.Env, in.TaskID); err != nil {
		return nil, err
	}
	a = analyzeDir(in.Module.Dir)
	st := p.depState(ctx, a)
	return &sdk.InstallDepsResult{Dependencies: st}, nil
}

// installAll：确保环境存在，再装依赖。console 为任务或运行的控制台。
func (p *Plugin) installAll(ctx *sdk.Context, a *analysis, tc *sdk.Toolchain, env map[string]string, console string) error {
	if a.manager == mgrNone && !a.pf.hasDeclarations() {
		ctx.Host.Console(console, ctx.T("step.nothingToInstall"))
		return nil
	}
	if a.manager == mgrConda {
		return p.installConda(ctx, a, env, console)
	}
	t, err := p.ensureVenv(ctx, a, tc, env, console)
	if err != nil {
		return err
	}
	return p.installInto(ctx, a, t, env, console)
}

// ensureVenv 返回可用的虚拟环境；不存在就用 uv 在工程里建 .venv。
func (p *Plugin) ensureVenv(ctx *sdk.Context, a *analysis, tc *sdk.Toolchain, env map[string]string, console string) (*envTarget, error) {
	if a.venv != nil {
		if a.venv.Broken != "" {
			return nil, sdk.UserError("%s", ctx.T("err.venvBroken", rel(a.dir, a.venv.Dir)))
		}
		return &envTarget{Venv: a.venv, Python: a.venv.Python}, nil
	}
	uv, err := p.ensureUV(ctx, console)
	if err != nil {
		return nil, err
	}
	target := filepath.Join(a.dir, ".venv")
	if tc == nil {
		tc = p.findToolchain(ctx, a.req.Constraint, a.req.Preferred)
	}
	args := []string{"venv", target}
	if tc != nil && tc.Executable != "" {
		args = append(args, "--python", tc.Executable)
	} else if a.req.Constraint != "" {
		// 没有满足要求的本机解释器：交给 uv 自动下载（UV_PYTHON_INSTALL_MIRROR 已由内核注入）
		args = append(args, "--python", uvPythonRequest(a.req))
	}
	r, err := ctx.Host.Exec(ctx, &sdk.ExecParams{Command: uv, Args: args, Cwd: a.dir, Env: uvEnv(env), Console: console, Title: ctx.T("step.createVenv")})
	if err != nil {
		return nil, err
	}
	if r.ExitCode != 0 {
		return nil, sdk.UserError("%s", ctx.T("err.createVenv", r.ExitCode))
	}
	v := inspectVenv(target)
	if v == nil || v.Broken != "" {
		return nil, sdk.UserError("%s", ctx.T("err.createVenv", r.ExitCode))
	}
	a.venv = v
	return &envTarget{Venv: v, Python: v.Python, Base: tc}, nil
}

// uvPythonRequest 把约束转成 uv 的 --python 请求。
func uvPythonRequest(r pythonReq) string {
	if r.Preferred != "" {
		return r.Preferred
	}
	c := r.Constraint
	if !strings.ContainsAny(c, "<>=!~^") && c != "" {
		return c
	}
	return strings.ReplaceAll(c, " ", ",")
}

// uvEnv 在内核环境之上补 uv 需要的变量。
func uvEnv(env map[string]string) map[string]string {
	out := sdk.MergeEnv(env)
	out["UV_NO_PROGRESS"] = "1"
	return out
}

// installInto 按包管理器把依赖装进虚拟环境。
func (p *Plugin) installInto(ctx *sdk.Context, a *analysis, t *envTarget, env map[string]string, console string) error {
	uv, err := p.ensureUV(ctx, console)
	if err != nil {
		return err
	}
	venvEnv := uvEnv(env)
	venvEnv["VIRTUAL_ENV"] = t.Venv.Dir
	pathPrepend := []string{binDir(t.Venv.Dir)}
	pip := func(title string, args ...string) error {
		full := append([]string{"pip", "install", "--python", t.Python}, args...)
		return p.exec(ctx, console, title, a.dir, venvEnv, pathPrepend, uv, full...)
	}
	title := ctx.T("step.installDeps")
	var runErr error
	switch {
	case a.manager == mgrUV && a.pf.UVLock:
		e := sdk.MergeEnv(venvEnv)
		e["UV_PROJECT_ENVIRONMENT"] = t.Venv.Dir
		runErr = p.exec(ctx, console, title, a.dir, e, pathPrepend, uv, "sync", "--frozen", "--inexact")
	case a.manager == mgrPoetry && a.pf.PoetryLock, a.manager == mgrPDM && a.pf.PDMLock:
		// 不让 poetry / pdm 自己挑解释器（POETRY_VIRTUALENVS_CREATE=false 时它会装进自身的基础解释器）：
		// 只用它们把锁文件导出成带环境标记的 requirements，再由 uv 装进我们的虚拟环境。锁文件不变。
		file, err := p.exportLock(ctx, a, uv, env, console)
		if err != nil {
			return err
		}
		runErr = pip(title, "-r", file)
	case a.manager == mgrPipenv:
		// 不依赖 pipenv：把 Pipfile.lock（或 Pipfile）转成临时 requirements 交给 uv
		var reqs []requirement
		if a.pf.PipfileLock {
			reqs = pipfileLockRequirements(filepath.Join(a.dir, "Pipfile.lock"))
		} else {
			reqs = pipfileRequirements(filepath.Join(a.dir, "Pipfile"), true)
		}
		file, err := p.writeTempRequirements(ctx, a.dir, reqs)
		if err != nil {
			return err
		}
		runErr = pip(title, "-r", file)
	case a.manager == mgrPoetry:
		// 没有 poetry.lock：不能跑 poetry install（会生成锁文件），转成 requirements 交给 uv
		reqs := poetryRequirements(a.pf.Pyproject.Tool.Poetry.Dependencies)
		file, err := p.writeTempRequirements(ctx, a.dir, reqs)
		if err != nil {
			return err
		}
		runErr = pip(title, "-r", file)
	default:
		files := a.installFiles()
		if len(files) == 0 {
			ctx.Host.Console(console, ctx.T("step.nothingToInstall"))
			return nil
		}
		var args []string
		for _, f := range files {
			args = append(args, "-r", f)
		}
		runErr = pip(title, args...)
	}
	if runErr != nil {
		return runErr
	}
	p.saveInstallRecord(ctx, a, t.Venv.Dir)
	return nil
}

// exportLock 用 poetry export / pdm export 把锁文件导出到 CacheDir（只读锁文件，不建环境、不联网解析）。
func (p *Plugin) exportLock(ctx *sdk.Context, a *analysis, uv string, env map[string]string, console string) (string, error) {
	out, err := p.tempRequirementsPath(ctx, a.dir)
	if err != nil {
		return "", err
	}
	e := uvEnv(env)
	var args []string
	if a.manager == mgrPoetry {
		e["POETRY_VIRTUALENVS_CREATE"] = "false"
		e["POETRY_NO_INTERACTION"] = "1"
		args = []string{"tool", "run", "--from", "poetry", "--with", "poetry-plugin-export", "poetry", "export", "--without-hashes", "--all-groups", "--all-extras", "-f", "requirements.txt", "-o", out}
	} else {
		e["PDM_CHECK_UPDATE"] = "false"
		e["PDM_IGNORE_ACTIVE_VENV"] = "1"
		args = []string{"tool", "run", "pdm", "export", "--no-hashes", "-G", ":all", "-o", out}
	}
	if err := p.exec(ctx, console, ctx.T("step.exportLock", a.pf.lockFile()), a.dir, e, nil, uv, args...); err != nil {
		return "", err
	}
	return out, nil
}

// installConda：environment.yml → conda env create / update（具名环境在 conda 自己的 envs 目录里）。
func (p *Plugin) installConda(ctx *sdk.Context, a *analysis, env map[string]string, console string) error {
	conda := condaExecutable()
	if conda == "" {
		return sdk.UserError("%s", ctx.T("err.condaMissing"))
	}
	file := filepath.Join(a.dir, a.pf.EnvYml)
	name, _ := envYmlInfo(file)
	var args []string
	if a.venv != nil && a.venv.Conda {
		args = []string{"env", "update", "--file", file, "--prefix", a.venv.Dir}
	} else if name != "" {
		args = []string{"env", "create", "--yes", "--file", file, "--name", name}
	} else {
		args = []string{"env", "create", "--yes", "--file", file, "--prefix", filepath.Join(a.dir, ".conda")}
	}
	if err := p.exec(ctx, console, ctx.T("step.installDeps"), a.dir, env, nil, conda, args...); err != nil {
		return err
	}
	na := analyzeDir(a.dir)
	if na.venv != nil {
		p.saveInstallRecord(ctx, na, na.venv.Dir)
	}
	return nil
}

// installPackages 往虚拟环境装指定的包（修复「缺模块」、动作「安装包」用）。
func (p *Plugin) installPackages(ctx *sdk.Context, dir string, pkgs []string, tc *sdk.Toolchain, env map[string]string, console string) error {
	a := analyzeDir(dir)
	if a.manager == mgrConda && a.venv == nil {
		return sdk.UserError("%s", ctx.T("err.condaEnvMissing"))
	}
	t, err := p.ensureVenv(ctx, a, tc, env, console)
	if err != nil {
		return err
	}
	uv, err := p.ensureUV(ctx, console)
	if err != nil {
		return err
	}
	args := append([]string{"pip", "install", "--python", t.Python}, pkgs...)
	return p.exec(ctx, console, ctx.T("step.installPackages", strings.Join(pkgs, " ")), dir, uvEnv(env), []string{binDir(t.Venv.Dir)}, uv, args...)
}

// writeTempRequirements 把依赖写进 CacheDir 下的临时 requirements 文件（不写工程目录）。
func (p *Plugin) writeTempRequirements(ctx *sdk.Context, dir string, reqs []requirement) (string, error) {
	var b strings.Builder
	for _, r := range reqs {
		if r.Local {
			continue
		}
		b.WriteString(r.Spec)
		b.WriteByte('\n')
	}
	path, err := p.tempRequirementsPath(ctx, dir)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// tempRequirementsPath 是 CacheDir 下这个模块的临时 requirements 文件。
func (p *Plugin) tempRequirementsPath(ctx *sdk.Context, dir string) (string, error) {
	cache := os.TempDir()
	if ctx.Init != nil && ctx.Init.CacheDir != "" {
		cache = ctx.Init.CacheDir
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(cache, "requirements-"+sdk.StableID(dir)+".txt"), nil
}

// exec 执行命令，输出进控制台；非零退出返回用户可读错误。
func (p *Plugin) exec(ctx *sdk.Context, console, title, cwd string, env map[string]string, pathPrepend []string, cmd string, args ...string) error {
	r, err := ctx.Host.Exec(ctx, &sdk.ExecParams{Command: cmd, Args: args, Cwd: cwd, Env: env, PathPrepend: pathPrepend, Console: console, Title: title, Capture: console == ""})
	if err != nil {
		return err
	}
	if r.Cancelled {
		return sdk.NewError(sdk.ErrCodeCancelled, "cancelled")
	}
	if r.ExitCode != 0 {
		if console == "" {
			return sdk.UserError("%s", ctx.T("err.stepFailedOutput", title, r.ExitCode, tailText(r.Stderr+r.Stdout, 1500)))
		}
		return sdk.UserError("%s", ctx.T("err.stepFailed", title, r.ExitCode))
	}
	return nil
}

func tailText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// ---------------------------------------------------------------------------
// 安装记录：记下依赖文件的哈希，用来判断「已安装 / 依赖文件改过」

type installRecord struct {
	Hash string    `json:"hash"`
	Venv string    `json:"venv"`
	Time time.Time `json:"time"`
}

func recordPath(ctx *sdk.Context, venv string) string {
	if ctx == nil || ctx.Init == nil || ctx.Init.DataDir == "" {
		return ""
	}
	return filepath.Join(ctx.Init.DataDir, "installs", sdk.StableID(venv)+".json")
}

func (p *Plugin) saveInstallRecord(ctx *sdk.Context, a *analysis, venv string) {
	path := recordPath(ctx, venv)
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	b, _ := json.Marshal(installRecord{Hash: a.depHash(), Venv: venv, Time: time.Now()})
	_ = os.WriteFile(path, b, 0o644)
}

func loadInstallRecord(ctx *sdk.Context, venv string) *installRecord {
	path := recordPath(ctx, venv)
	if path == "" {
		return nil
	}
	var r installRecord
	if err := sdk.ReadJSON(path, &r); err != nil {
		return nil
	}
	return &r
}

// depHash 是依赖声明文件（含锁文件）内容的哈希。
func (a *analysis) depHash() string {
	h := sha1.New()
	for _, f := range a.depFiles() {
		b, _ := os.ReadFile(filepath.Join(a.dir, f))
		h.Write([]byte(f))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// depState 计算依赖状态：环境不存在或缺声明的包 → missing；
// 包都在、但上次安装之后依赖文件改过 → outdated；否则 ok。
func (p *Plugin) depState(ctx *sdk.Context, a *analysis) sdk.DependencyState {
	st := sdk.DependencyState{Manager: a.manager}
	if a.manager == mgrNone && !a.pf.hasDeclarations() {
		st.State = "none"
		return st
	}
	if a.venv == nil {
		st.State = "missing"
		if a.manager == mgrConda {
			st.Message = ctx.T("deps.condaEnvMissing")
		} else {
			st.Message = ctx.T("deps.noVenv")
		}
		return st
	}
	if a.venv.Broken != "" {
		st.State = "missing"
		st.Message = ctx.T("deps.venvBroken")
		return st
	}
	rec := loadInstallRecord(ctx, a.venv.Dir)
	hash := a.depHash()
	missing := a.missingPackages()
	switch {
	case len(missing) > 0:
		st.State = "missing"
		show := missing
		if len(show) > 3 {
			show = show[:3]
		}
		st.Message = ctx.T("deps.missingPackages", strings.Join(show, ", "), len(missing))
	case rec != nil && rec.Hash != hash:
		st.State = "outdated"
		st.Message = ctx.T("deps.changed")
	default:
		st.State = "ok"
	}
	return st
}

// missingPackages 列出声明了但环境里没有的包（按当前平台求值环境标记）。
func (a *analysis) missingPackages() []string {
	if a.venv == nil {
		return nil
	}
	installed := installedDists(a.venv.Dir)
	env := markerEnvFor(a.venv.Version)
	var out []string
	seen := map[string]bool{}
	for _, r := range a.pf.declaredDeps() {
		if r.Local || r.Name == "" || seen[r.Name] {
			continue
		}
		seen[r.Name] = true
		if !evalMarker(r.Marker, env) {
			continue
		}
		if _, ok := installed[r.Name]; !ok {
			out = append(out, r.Name)
		}
	}
	return out
}

// markerEnvFor 构造当前平台的环境标记变量。
func markerEnvFor(pyVersion string) markerEnv {
	if pyVersion == "" {
		pyVersion = "3.12"
	}
	short := pyVersion
	if parts := strings.Split(pyVersion, "."); len(parts) >= 2 {
		short = parts[0] + "." + parts[1]
	}
	env := markerEnv{
		"python_version":                 short,
		"python_full_version":            pyVersion,
		"implementation_name":            "cpython",
		"platform_python_implementation": "CPython",
		"implementation_version":         pyVersion,
	}
	switch runtime.GOOS {
	case "darwin":
		env["sys_platform"], env["platform_system"], env["os_name"] = "darwin", "Darwin", "posix"
	case "windows":
		env["sys_platform"], env["platform_system"], env["os_name"] = "win32", "Windows", "nt"
	default:
		env["sys_platform"], env["platform_system"], env["os_name"] = "linux", "Linux", "posix"
	}
	switch runtime.GOARCH {
	case "arm64":
		if runtime.GOOS == "linux" {
			env["platform_machine"] = "aarch64"
		} else if runtime.GOOS == "windows" {
			env["platform_machine"] = "ARM64"
		} else {
			env["platform_machine"] = "arm64"
		}
	case "amd64":
		if runtime.GOOS == "windows" {
			env["platform_machine"] = "AMD64"
		} else {
			env["platform_machine"] = "x86_64"
		}
	}
	return env
}
