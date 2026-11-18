package python

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// packageRow 是一个已安装包。
type packageRow struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Declared bool   `json:"declared"` // 是工程直接声明的依赖
}

// listPackages 读虚拟环境 site-packages 里的 dist-info（不执行命令，毫秒级）。
func listPackages(a *analysis) []packageRow {
	if a.venv == nil {
		return nil
	}
	declared := map[string]bool{}
	for _, r := range a.pf.declaredDeps() {
		declared[r.Name] = true
	}
	var out []packageRow
	for norm, d := range installedDists(a.venv.Dir) {
		out = append(out, packageRow{Name: d.Name, Version: d.Version, Declared: declared[norm]})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func moduleDir(m *sdk.ModuleInfo, prj *sdk.ProjectInfo) string {
	if m != nil && m.Dir != "" {
		return m.Dir
	}
	if prj != nil {
		return prj.Root
	}
	return ""
}

// RenderView 渲染「包」标签页。
func (p *Plugin) RenderView(ctx *sdk.Context, in *sdk.ViewRenderParams) (*sdk.ViewRenderResult, error) {
	if in.View != "packages" {
		return nil, sdk.ErrNotSupported
	}
	dir := moduleDir(in.Module, in.Project)
	if dir == "" {
		return &sdk.ViewRenderResult{Root: sdk.Empty(ctx.T("view.noModule"), "")}, nil
	}
	a := analyzeDir(dir)
	if a.venv == nil {
		empty := sdk.ViewNode{Type: "empty", Icon: "builtin:package", Title: ctx.T("view.noVenv"), Children: []sdk.ViewNode{
			{Type: "button", Text: ctx.T("action.installDeps"), Action: "install", Style: "primary", Icon: "builtin:download"},
		}}
		return &sdk.ViewRenderResult{Root: empty}, nil
	}
	filter := ""
	if in.State != nil {
		filter, _ = in.State["filter"].(string)
	}
	rows := listPackages(a)
	var tableRows []sdk.TableRow
	for _, r := range rows {
		if filter != "" && !strings.Contains(strings.ToLower(r.Name), strings.ToLower(filter)) {
			continue
		}
		row := sdk.TableRow{ID: normName(r.Name), Cells: map[string]string{"name": r.Name, "version": r.Version}}
		if r.Declared {
			row.Cells["declared"] = ctx.T("view.declared")
		}
		tableRows = append(tableRows, row)
	}
	st := p.depState(ctx, a)
	tone := "success"
	if st.State != "ok" {
		tone = "warning"
	}
	var stateText string
	switch st.State {
	case "ok":
		stateText = ctx.T("deps.ok")
	default:
		stateText = st.Message
	}
	items := []sdk.KV{
		{Key: ctx.T("view.interpreter"), Value: a.venv.Python, Mono: true},
		{Key: "Python", Value: a.venv.Version},
		{Key: ctx.T("info.manager"), Value: managerLabel(a.manager)},
		{Key: ctx.T("view.depState"), Value: stateText, Tone: tone},
	}
	root := sdk.Column(
		sdk.ViewNode{Type: "toolbar", Children: []sdk.ViewNode{
			{Type: "button", Text: ctx.T("action.installDeps"), Icon: "builtin:download", Action: "install", Style: "primary"},
			{Type: "button", Text: ctx.T("action.refresh"), Icon: "builtin:refresh-cw", Action: "refresh", Style: "secondary"},
		}},
		sdk.KVList(items...),
		sdk.Form("filter", ctx.T("view.filter"), []sdk.FormField{{Key: "filter", Title: ctx.T("view.filter"), Type: "string", Placeholder: ctx.T("view.filterPlaceholder")}}, map[string]any{"filter": filter}),
		sdk.ViewNode{Type: "section", Title: ctx.T("view.packagesCount", strconv.Itoa(len(rows))), Children: []sdk.ViewNode{
			sdk.Table([]sdk.TableColumn{
				{Key: "name", Title: ctx.T("view.colName")},
				{Key: "version", Title: ctx.T("view.colVersion"), Mono: true, Width: 140},
				{Key: "declared", Title: ctx.T("view.colDeclared"), Width: 100},
			}, tableRows),
		}},
		sdk.Form("installPackage", ctx.T("action.install"), []sdk.FormField{{Key: "packages", Title: ctx.T("action.installPackage"), Type: "string", Placeholder: "requests==2.32.3", Info: ctx.T("info.installPackage")}}, nil),
	)
	return &sdk.ViewRenderResult{Root: root, State: map[string]any{"filter": filter}}, nil
}

// HandleViewEvent 处理「包」标签页的事件。
func (p *Plugin) HandleViewEvent(ctx *sdk.Context, in *sdk.ViewEventParams) (*sdk.ViewEventResult, error) {
	dir := moduleDir(in.Module, in.Project)
	state := in.State
	if state == nil {
		state = map[string]any{}
	}
	switch in.Event {
	case "refresh":
		return &sdk.ViewEventResult{Refresh: true, State: state}, nil
	case "filter":
		state["filter"], _ = in.Values["filter"].(string)
		return &sdk.ViewEventResult{Refresh: true, State: state}, nil
	case "install":
		if err := p.installAll(ctx, analyzeDir(dir), nil, nil, in.TaskID); err != nil {
			return nil, err
		}
		return &sdk.ViewEventResult{Refresh: true, State: state, Message: ctx.T("msg.depsInstalled"), Reimport: true}, nil
	case "installPackage":
		raw, _ := in.Values["packages"].(string)
		pkgs := strings.Fields(strings.ReplaceAll(raw, ",", " "))
		if len(pkgs) == 0 {
			return &sdk.ViewEventResult{Message: ctx.T("err.noPackage")}, nil
		}
		if err := p.installPackages(ctx, dir, pkgs, nil, nil, in.TaskID); err != nil {
			return nil, err
		}
		return &sdk.ViewEventResult{Refresh: true, State: state, Message: ctx.T("msg.installed", strings.Join(pkgs, " "))}, nil
	}
	return nil, sdk.ErrNotSupported
}

// CallTool 执行 MCP 工具 packages：列出模块虚拟环境里的包与环境信息。
func (p *Plugin) CallTool(ctx *sdk.Context, in *sdk.ToolCallParams) (any, error) {
	if in.Tool != "packages" {
		return nil, sdk.ErrNotSupported
	}
	var args struct {
		Filter string `json:"filter"`
	}
	if len(in.Args) > 0 {
		_ = json.Unmarshal(in.Args, &args)
	}
	dir := moduleDir(in.Module, in.Project)
	if dir == "" {
		return nil, sdk.UserError("moduleId is required")
	}
	a := analyzeDir(dir)
	st := p.depState(ctx, a)
	res := map[string]any{
		"manager":         a.manager,
		"dependencyState": st.State,
		"declared":        a.model().Dependencies,
	}
	if a.req.Constraint != "" {
		res["requiresPython"] = a.req.Constraint
	}
	if st.Message != "" {
		res["dependencyMessage"] = st.Message
	}
	if a.venv == nil {
		res["venv"] = nil
		res["packages"] = []packageRow{}
		return res, nil
	}
	res["venv"] = a.venv.Dir
	res["python"] = a.venv.Python
	res["pythonVersion"] = a.venv.Version
	if a.venv.Broken != "" {
		res["venvBroken"] = true
	}
	var pkgs []packageRow
	for _, r := range listPackages(a) {
		if args.Filter == "" || strings.Contains(strings.ToLower(r.Name), strings.ToLower(args.Filter)) {
			pkgs = append(pkgs, r)
		}
	}
	if pkgs == nil {
		pkgs = []packageRow{}
	}
	res["packages"] = pkgs
	return res, nil
}
