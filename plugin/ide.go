package python

import (
	"encoding/json"
	"path/filepath"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// ideConfigs 把 PyCharm（PythonConfigurationType）与 VS Code（debugpy / python）运行配置转成 python-script。
func ideConfigs(in *sdk.AnalyzeParams) []sdk.RunConfiguration {
	var out []sdk.RunConfiguration
	root := in.Project.Root
	expand := func(s string) string {
		for _, v := range []string{"$PROJECT_DIR$", "${workspaceFolder}", "${workspaceRoot}"} {
			s = strings.ReplaceAll(s, v, root)
		}
		s = strings.ReplaceAll(s, "$MODULE_DIR$", in.Module.Dir)
		return s
	}
	relToModule := func(p string) string {
		p = expand(p)
		if p == "" {
			return ""
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		if r, err := filepath.Rel(in.Module.Dir, p); err == nil && !strings.HasPrefix(r, "..") {
			return filepath.ToSlash(r)
		}
		return p
	}
	for _, c := range in.IdeConfigs {
		opts := map[string]any{}
		switch {
		case c.Source == "idea" && c.Type == "PythonConfigurationType":
			script := c.Options["SCRIPT_NAME"]
			if script == "" {
				continue
			}
			if c.Options["MODULE_MODE"] == "true" {
				opts["module"] = script
			} else {
				opts["script"] = relToModule(script)
			}
			if v := c.Options["PARAMETERS"]; v != "" {
				opts["args"] = expand(v)
			}
			if v := c.Options["INTERPRETER_OPTIONS"]; v != "" {
				opts["interpreterOptions"] = v
			}
			if wd := relToModule(c.Options["WORKING_DIRECTORY"]); wd != "" && wd != "." {
				opts["workingDir"] = wd
			}
			if v := c.Options["ENV_FILES"]; v != "" {
				opts["envFile"] = relToModule(strings.Split(v, ";")[0])
			}
		case c.Source == "vscode" && (c.Type == "debugpy" || c.Type == "python"):
			if c.Options["request"] != "" && c.Options["request"] != "launch" {
				continue
			}
			switch {
			case c.Options["module"] != "":
				opts["module"] = c.Options["module"]
			case c.Options["program"] != "" && !strings.Contains(c.Options["program"], "${file}"):
				opts["script"] = relToModule(c.Options["program"])
			default:
				continue
			}
			if v := c.Options["args"]; v != "" {
				opts["args"] = vscodeArgs(v)
			}
			if wd := relToModule(c.Options["cwd"]); wd != "" && wd != "." {
				opts["workingDir"] = wd
			}
			if v := c.Options["envFile"]; v != "" {
				opts["envFile"] = relToModule(v)
			}
		default:
			continue
		}
		rc := sdk.RunConfiguration{
			ID:       sdk.StableID(c.Source, c.Type, c.Name),
			Name:     c.Name,
			Type:     "python-script",
			Options:  opts,
			Env:      c.Env,
			Source:   c.Source,
			Priority: 60,
		}
		out = append(out, rc)
	}
	return out
}

// vscodeArgs 把 launch.json 的 args（JSON 数组或字符串）转成一行参数。
func vscodeArgs(v string) string {
	var list []string
	if strings.HasPrefix(strings.TrimSpace(v), "[") && json.Unmarshal([]byte(v), &list) == nil {
		for i, a := range list {
			if strings.ContainsAny(a, " \t\"'") {
				list[i] = pyQuote(a)
			}
		}
		return strings.Join(list, " ")
	}
	return v
}
