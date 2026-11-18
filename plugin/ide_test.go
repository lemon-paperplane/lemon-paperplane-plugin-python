package python

import (
	"testing"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// TestIdeaTemporaryConfig 对应 sudoku-api 里 PyCharm 的临时运行配置（带 ENV_FILES=.env）。
func TestIdeaTemporaryConfig(t *testing.T) {
	in := &sdk.AnalyzeParams{
		Project: sdk.ProjectInfo{Root: "/p"},
		Module:  sdk.ModuleInfo{Dir: "/p"},
		IdeConfigs: []sdk.IdeRunConfig{{
			Source: "idea", Type: "PythonConfigurationType", Name: "main",
			Options: map[string]string{"SCRIPT_NAME": "$PROJECT_DIR$/main.py", "ENV_FILES": "$PROJECT_DIR$/.env", "WORKING_DIRECTORY": "$PROJECT_DIR$", "MODULE_MODE": "false", "PARAMETERS": ""},
			Env:     map[string]string{"PYTHONUNBUFFERED": "1"},
		}, {
			Source: "vscode", Type: "debugpy", Name: "api",
			Options: map[string]string{"request": "launch", "module": "uvicorn", "args": `["main:app", "--port", "9000"]`, "cwd": "${workspaceFolder}"},
		}},
	}
	got := ideConfigs(in)
	if len(got) != 2 {
		t.Fatalf("configs = %+v", got)
	}
	o := got[0].Options
	if o["script"] != "main.py" || o["envFile"] != ".env" || o["workingDir"] != nil || got[0].Source != "idea" || got[0].Env["PYTHONUNBUFFERED"] != "1" {
		t.Errorf("idea = %+v", got[0])
	}
	o = got[1].Options
	if o["module"] != "uvicorn" || o["args"] != "main:app --port 9000" {
		t.Errorf("vscode = %+v", got[1])
	}
}
