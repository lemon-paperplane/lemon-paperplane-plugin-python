package python

import (
	"os"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// ApplyFix 处理 custom 修复项。内核把修复放在任务里执行，长命令的输出进任务控制台（TaskID）。
func (p *Plugin) ApplyFix(ctx *sdk.Context, in *sdk.ApplyFixParams) (*sdk.ApplyFixResult, error) {
	dir := in.Module.Dir
	var tc *sdk.Toolchain
	if t, ok := in.Toolchains["python"]; ok {
		tc = &t
	}
	switch in.Fix.ID {
	case "recreateVenv":
		// 删掉坏的/版本不对的虚拟环境，用选定的 Python 重建并装依赖
		a := analyzeDir(dir)
		if a.venv != nil && a.venv.Kind == "project" {
			ctx.Host.Console(in.TaskID, ctx.T("step.removeVenv", rel(dir, a.venv.Dir)))
			if err := os.RemoveAll(a.venv.Dir); err != nil {
				return nil, err
			}
		}
		if err := p.installAll(ctx, analyzeDir(dir), tc, in.Env, in.TaskID); err != nil {
			return nil, err
		}
		return &sdk.ApplyFixResult{Rerun: true, Changes: &sdk.ProjectChanges{Reimport: true}, Message: ctx.T("msg.venvCreated")}, nil
	case "installPackage", "setuptools":
		mod := in.Fix.Params["module"]
		if mod == "" {
			return nil, sdk.UserError("%s", ctx.T("err.noPackage"))
		}
		pkg := packageForImport(mod)
		if err := p.installPackages(ctx, dir, []string{pkg}, tc, in.Env, in.TaskID); err != nil {
			return nil, err
		}
		return &sdk.ApplyFixResult{Rerun: true, Message: ctx.T("msg.installed", pkg)}, nil
	case "createVenv":
		if err := p.installAll(ctx, analyzeDir(dir), tc, in.Env, in.TaskID); err != nil {
			return nil, err
		}
		return &sdk.ApplyFixResult{Rerun: true, Changes: &sdk.ProjectChanges{Reimport: true}, Message: ctx.T("msg.venvCreated")}, nil
	case "poetryLock":
		uv, err := p.ensureUV(ctx, in.TaskID)
		if err != nil {
			return nil, err
		}
		env := sdk.MergeEnv(in.Env, map[string]string{"POETRY_NO_INTERACTION": "1", "POETRY_VIRTUALENVS_CREATE": "false"})
		if idx := p.mirrors(ctx).PypiIndex; idx != "" {
			env["POETRY_PYPI_MIRROR_URL"] = idx
		}
		if err := p.exec(ctx, in.TaskID, "poetry lock", dir, env, nil, uv, "tool", "run", "--from", "poetry", "--with", "poetry-plugin-pypi-mirror", "poetry", "lock"); err != nil {
			return nil, err
		}
		return &sdk.ApplyFixResult{Rerun: true, Changes: &sdk.ProjectChanges{Reimport: true}}, nil
	}
	return nil, sdk.ErrNotSupported
}

// InvokeAction 处理插件声明的动作。
func (p *Plugin) InvokeAction(ctx *sdk.Context, in *sdk.InvokeActionParams) (*sdk.InvokeActionResult, error) {
	dir := in.Module.Dir
	var tc *sdk.Toolchain
	if t, ok := in.Toolchains["python"]; ok {
		tc = &t
	}
	switch in.Action {
	case "recreateVenv":
		a := analyzeDir(dir)
		if a.venv != nil && a.venv.Kind == "project" {
			ctx.Host.Console(in.TaskID, ctx.T("step.removeVenv", rel(dir, a.venv.Dir)))
			if err := os.RemoveAll(a.venv.Dir); err != nil {
				return nil, err
			}
		}
		if err := p.installAll(ctx, analyzeDir(dir), tc, in.Env, in.TaskID); err != nil {
			return nil, err
		}
		return &sdk.InvokeActionResult{Message: ctx.T("msg.venvCreated"), Reimport: true}, nil
	case "installPackage":
		raw, _ := in.Args["packages"].(string)
		pkgs := strings.Fields(strings.ReplaceAll(raw, ",", " "))
		if len(pkgs) == 0 {
			return nil, sdk.UserError("%s", ctx.T("err.noPackage"))
		}
		if err := p.installPackages(ctx, dir, pkgs, tc, in.Env, in.TaskID); err != nil {
			return nil, err
		}
		return &sdk.InvokeActionResult{Message: ctx.T("msg.installed", strings.Join(pkgs, " ")), Reimport: true}, nil
	}
	return nil, sdk.ErrNotSupported
}
