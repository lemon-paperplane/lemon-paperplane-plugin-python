package python

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// ResolveEnvParams 是服务 resolveEnv 的参数（django / fastapi / flask 插件经 host/call 调用）。
type ResolveEnvParams struct {
	Module     sdk.ModuleInfo           `json:"module"`
	Toolchains map[string]sdk.Toolchain `json:"toolchains,omitempty"`
	Env        map[string]string        `json:"env,omitempty"`     // 内核给的环境增量（镜像 + 工程 + 运行配置）
	Console    string                   `json:"console,omitempty"` // 运行 id：建环境、补装依赖的输出进这里
	Ensure     bool                     `json:"ensure,omitempty"`  // 环境不存在时创建并安装依赖
	EnvFile    *string                  `json:"envFile,omitempty"` // .env 文件（相对模块目录）；nil 表示默认 ".env"，"" 表示不读
}

// ResolveEnvResult 告诉调用方用哪个解释器、补哪些环境变量。
type ResolveEnvResult struct {
	Python      string            `json:"python"`
	Env         map[string]string `json:"env"`
	PathPrepend []string          `json:"pathPrepend,omitempty"`
	Venv        string            `json:"venv,omitempty"`
	Version     string            `json:"version,omitempty"`
}

// CallService 处理其他插件的 host/call。
func (p *Plugin) CallService(ctx *sdk.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "resolveEnv":
		var in ResolveEnvParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, sdk.NewError(sdk.ErrCodeInvalidParams, "%v", err)
		}
		return p.resolveEnv(ctx, &in)
	case "installPackages":
		var in struct {
			Module     sdk.ModuleInfo           `json:"module"`
			Toolchains map[string]sdk.Toolchain `json:"toolchains,omitempty"`
			Env        map[string]string        `json:"env,omitempty"`
			Console    string                   `json:"console,omitempty"`
			Packages   []string                 `json:"packages"`
		}
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, sdk.NewError(sdk.ErrCodeInvalidParams, "%v", err)
		}
		var tc *sdk.Toolchain
		if t, ok := in.Toolchains["python"]; ok {
			tc = &t
		}
		return map[string]bool{"ok": true}, p.installPackages(ctx, in.Module.Dir, in.Packages, tc, in.Env, in.Console)
	}
	return nil, sdk.ErrNotSupported
}

// resolveEnv 决定运行用的解释器与环境变量；Ensure 时自动建虚拟环境、补装依赖（相当于 IDEA 的 Before launch）。
func (p *Plugin) resolveEnv(ctx *sdk.Context, in *ResolveEnvParams) (*ResolveEnvResult, error) {
	a := analyzeDir(in.Module.Dir)
	var tc *sdk.Toolchain
	if t, ok := in.Toolchains["python"]; ok {
		tc = &t
	}
	needsEnv := a.manager == mgrConda || a.pf.hasDeclarations()
	if in.Ensure && needsEnv {
		if a.venv == nil {
			ctx.Host.Console(in.Console, ctx.T("step.firstRunSetup"))
			if err := p.installAll(ctx, a, tc, in.Env, in.Console); err != nil {
				return nil, err
			}
			a = analyzeDir(in.Module.Dir)
		} else if a.venv.Broken != "" {
			return nil, sdk.UserError("%s", ctx.T("err.venvBroken", rel(a.dir, a.venv.Dir)))
		} else if st := p.depState(ctx, a); st.State == "missing" {
			// 环境在、缺包：补装一次；失败不拦运行（可能只是可选平台包装不上），交给运行时诊断
			ctx.Host.Console(in.Console, ctx.T("step.depsMissing", st.Message))
			if err := p.installAll(ctx, a, tc, in.Env, in.Console); err != nil {
				ctx.Host.Console(in.Console, ctx.T("step.depsInstallFailedContinue"))
			}
			a = analyzeDir(in.Module.Dir)
		}
	}
	res := &ResolveEnvResult{Env: map[string]string{}}
	switch {
	case a.venv != nil && a.venv.Broken == "":
		res.Python = a.venv.Python
		res.Venv = a.venv.Dir
		res.Version = a.venv.Version
		if a.venv.Conda {
			res.Env["CONDA_PREFIX"] = a.venv.Dir
			res.Env["CONDA_DEFAULT_ENV"] = filepath.Base(a.venv.Dir)
			res.PathPrepend = append(res.PathPrepend, binDir(a.venv.Dir))
			if runtime.GOOS == "windows" {
				res.PathPrepend = append([]string{a.venv.Dir, filepath.Join(a.venv.Dir, "Library", "bin")}, res.PathPrepend...)
			}
		} else {
			res.Env["VIRTUAL_ENV"] = a.venv.Dir
			res.PathPrepend = []string{binDir(a.venv.Dir)}
		}
	case tc != nil && tc.Executable != "":
		res.Python = tc.Executable
		res.Version = tc.Version
	default:
		if t := p.findToolchain(ctx, a.req.Constraint, a.req.Preferred); t != nil {
			res.Python, res.Version = t.Executable, t.Version
			res.PathPrepend = t.PathPrepend
		} else {
			return nil, sdk.UserError("%s", ctx.T("err.noInterpreter"))
		}
	}
	res.Env["PYTHONUNBUFFERED"] = "1"
	res.Env["PYTHONIOENCODING"] = "utf-8"
	// PYTHONPATH：内容根 + 源码根（与 PyCharm 默认一致），保留用户原有的
	roots := a.sourceRoots()
	if old := in.Env["PYTHONPATH"]; old != "" {
		roots = append(roots, old)
	} else if old := os.Getenv("PYTHONPATH"); old != "" {
		roots = append(roots, old)
	}
	res.Env["PYTHONPATH"] = strings.Join(roots, string(os.PathListSeparator))
	// .env：与 VS Code 一样默认读模块目录下的 .env；运行配置里显式写的环境变量优先
	envFile := ".env"
	if in.EnvFile != nil {
		envFile = *in.EnvFile
	}
	if envFile != "" {
		path := envFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(a.dir, path)
		}
		for k, v := range loadDotenv(path) {
			if _, set := in.Env[k]; !set {
				res.Env[k] = v
			}
		}
	}
	return res, nil
}

// PrepareRun 准备 python-script 运行配置。
func (p *Plugin) PrepareRun(ctx *sdk.Context, in *sdk.PrepareRunParams) (*sdk.LaunchSpec, error) {
	opts := in.Config.Options
	envFile := sdk.OptString(opts, "envFile", ".env")
	if v, ok := opts["envFile"]; ok && v == "" {
		envFile = ""
	}
	r, err := p.resolveEnv(ctx, &ResolveEnvParams{Module: in.Module, Toolchains: in.Toolchains, Env: in.Env, Console: in.RunID, Ensure: true, EnvFile: &envFile})
	if err != nil {
		return nil, err
	}
	script := sdk.OptString(opts, "script", "")
	module := sdk.OptString(opts, "module", "")
	var args []string
	args = append(args, sdk.SplitArgs(sdk.OptString(opts, "interpreterOptions", ""))...)
	switch {
	case module != "" && strings.Contains(module, ":"):
		// 入口点 "pkg.mod:func"：与 console_scripts 一样调用函数，返回值作为退出码
		mod, fn, _ := strings.Cut(module, ":")
		code := "import sys; sys.argv[0] = " + pyQuote(mod) + "; from " + mod + " import " + strings.SplitN(fn, ".", 2)[0] + " as _f; sys.exit(_f" + attrChain(fn) + "())"
		args = append(args, "-c", code)
	case module != "":
		args = append(args, "-m", module)
	case script != "":
		path := script
		if !filepath.IsAbs(path) {
			path = filepath.Join(in.Module.Dir, path)
		}
		if !sdk.FileExists(path) {
			return nil, sdk.UserError("%s", ctx.T("err.scriptNotFound", script))
		}
		args = append(args, path)
	default:
		return nil, sdk.UserError("%s", ctx.T("err.noScript"))
	}
	args = append(args, sdk.SplitArgs(sdk.OptString(opts, "args", ""))...)
	cwd := in.Module.Dir
	if wd := sdk.OptString(opts, "workingDir", ""); wd != "" {
		if filepath.IsAbs(wd) {
			cwd = wd
		} else {
			cwd = filepath.Join(in.Module.Dir, wd)
		}
	}
	env := r.Env
	for k, v := range optEnv(opts) {
		env[k] = v
	}
	return &sdk.LaunchSpec{
		Command:     r.Python,
		Args:        args,
		Cwd:         cwd,
		Env:         env,
		PathPrepend: r.PathPrepend,
	}, nil
}

// attrChain 把 "main.run" 里第一个名字之后的属性链拼回去（入口点可以是 obj.method）。
func attrChain(fn string) string {
	if i := strings.Index(fn, "."); i >= 0 {
		return fn[i:]
	}
	return ""
}

func pyQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// optEnv 读运行配置选项里的 env（keyvalue：对象或 "K=V" 列表）。
func optEnv(opts map[string]any) map[string]string {
	out := map[string]string{}
	switch t := opts["env"].(type) {
	case map[string]any:
		for k, v := range t {
			out[k] = toString(v)
		}
	case map[string]string:
		for k, v := range t {
			out[k] = v
		}
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok {
				if k, v, ok := strings.Cut(s, "="); ok {
					out[k] = v
				}
			}
		}
	}
	return out
}

var dotenvVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// loadDotenv 解析 .env（KEY=VALUE、export 前缀、引号、# 注释、${VAR} 引用）。文件不存在或是目录时返回空。
func loadDotenv(path string) map[string]string {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.Size() > 1<<20 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" || strings.ContainsAny(k, " \t") {
			continue
		}
		switch {
		case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
			v = v[1 : len(v)-1]
		case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
			v = strings.NewReplacer(`\n`, "\n", `\"`, `"`, `\\`, `\`).Replace(v[1 : len(v)-1])
			v = expandDotenv(v, out)
		default:
			if i := strings.Index(v, " #"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
			v = expandDotenv(v, out)
		}
		out[k] = v
	}
	return out
}

func expandDotenv(v string, vars map[string]string) string {
	return dotenvVarRe.ReplaceAllStringFunc(v, func(m string) string {
		sub := dotenvVarRe.FindStringSubmatch(m)
		if x, ok := vars[sub[1]]; ok {
			return x
		}
		if x := os.Getenv(sub[1]); x != "" {
			return x
		}
		return sub[2]
	})
}
