package python

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

func TestShouldDetect(t *testing.T) {
	root, _ := filepath.Abs("testdata/mixed")
	cases := map[string]bool{
		"":           true,  // requirements.txt + main.py
		"service":    true,  // 嵌套的独立工程（自己的 pyproject）
		"docs":       false, // 只有 requirements 的辅助目录
		"vendor/lib": false, // 第三方代码
		"pkg":        false, // 工程里的包
		"web":        false, // 前端目录里的辅助脚本
	}
	for rel, want := range cases {
		dir := filepath.Join(root, rel)
		got := shouldDetect(root, dir, rel, readProject(dir, nil))
		if got != want {
			t.Errorf("shouldDetect(%q) = %v, want %v", rel, got, want)
		}
	}
	loose, _ := filepath.Abs("testdata/loose")
	if !shouldDetect(loose, loose, "", readProject(loose, nil)) {
		t.Error("loose scripts with __main__ should be detected at root")
	}
}

func TestProjectClaims(t *testing.T) {
	got := projectClaims(readProject("testdata/mixed", nil))
	// docs、pkg 属于本工程；web 是前端（package.json）不认领；service 是独立工程不认领；vendor 由 SDK 默认跳过
	want := []string{"docs", "pkg"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("claims = %v, want %v", got, want)
	}
}

func TestReferencedPaths(t *testing.T) {
	pf := readProject("testdata/reqproj", nil)
	refs := pf.referencedPaths()
	if !refs.match("libs/shared") {
		t.Fatalf("refs = %v", refs)
	}
}

func TestMainScripts(t *testing.T) {
	got := mainScripts("testdata/loose", listDir("testdata/loose"), 5)
	if !reflect.DeepEqual(got, []string{"tool.py"}) {
		t.Fatalf("mainScripts = %v", got)
	}
}

func TestVenvInspect(t *testing.T) {
	dir := t.TempDir()
	venv := filepath.Join(dir, ".venv")
	os.MkdirAll(filepath.Join(venv, "lib", "python3.12", "site-packages", "requests-2.32.3.dist-info"), 0o755)
	os.MkdirAll(filepath.Join(venv, "lib", "python3.12", "site-packages", "PyYAML-6.0.dist-info"), 0o755)
	os.MkdirAll(filepath.Join(venv, "Lib", "site-packages"), 0o755)
	os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte("home = /nonexistent/python/bin\nversion_info = 3.12.4\n"), 0o644)
	v := inspectVenv(venv)
	if v == nil || v.Version != "3.12.4" || v.Broken != "home" {
		t.Fatalf("venv = %+v", v)
	}
	d := installedDists(venv)
	if _, ok := d["pyyaml"]; !ok && filepath.Separator == '/' {
		t.Fatalf("dists = %v", d)
	}
	if d["requests"].Version != "2.32.3" && filepath.Separator == '/' {
		t.Fatalf("requests = %+v", d["requests"])
	}
}

func TestSuggestPython(t *testing.T) {
	cases := map[string]string{
		"  ╰─▶ Because the current Python version (3.13.5) does not satisfy Python>=3.8,<3.12 and numpy==1.24.0 depends on Python>=3.8,<3.12, we can conclude": ">=3.8,<3.12",
		"hint: Wheels are available for `torch` (v2.0.0) with the following Python ABI tags: `cp38`, `cp39`, `cp310`, `cp311`":                                 "3.11",
		"Using Python 3.13.5 environment at: .venv\nERROR: No matching distribution found for open3d==0.19":                                                    "3.12",
		"nothing relevant": "",
	}
	for in, want := range cases {
		if got := suggestPython(in); got != want {
			t.Errorf("suggestPython(%q) = %q, want %q", in[:20], got, want)
		}
	}
	if packageForImport("cv2") != "opencv-python" || packageForImport("my_pkg") != "my-pkg" {
		t.Error("packageForImport")
	}
}

// TestManifestRules 校验 plugin.json 的规则都能编译，并用真实报错行检查命中与捕获组。
func TestManifestRules(t *testing.T) {
	var m sdk.Manifest
	b, err := os.ReadFile("../plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	rules := map[string]*regexp.Regexp{}
	hints := map[string]string{}
	for _, r := range m.Contributes.Diagnostics {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			t.Fatalf("%s: %v", r.ID, err)
		}
		if r.Hint == "" {
			t.Errorf("%s: no hint", r.ID)
		}
		rules[r.ID], hints[r.ID] = re, r.Hint
	}
	for _, u := range m.Contributes.URLPatterns {
		if _, err := regexp.Compile(u.Pattern); err != nil {
			t.Fatalf("url %s: %v", u.Pattern, err)
		}
	}
	// 真实日志行（来自本机实测与各工具的原始输出）
	lines := map[string][]string{
		"python.module-not-found":          {"ModuleNotFoundError: No module named 'yaml'", "yaml"},
		"python.distutils-removed":         {"ModuleNotFoundError: No module named 'distutils'"},
		"python.requires-different-python": {"ERROR: Package 'open3d' requires a different Python: 3.13.5 not in '<3.12,>=3.8'", "3.13.5", "<3.12,>=3.8"},
		"python.uv-python-unsatisfied":     {"  ╰─▶ Because the current Python version (3.13.5) does not satisfy Python>=3.8,<3.12 and open3d==0.18.0 depends on Python>=3.8,<3.12", "3.13.5", ">=3.8,<3.12"},
		"python.no-matching-distribution":  {"ERROR: No matching distribution found for open3d==0.19.0", "open3d==0.19.0"},
		"python.uvicorn-address-in-use":    {"ERROR:    [Errno 48] error while attempting to bind on address ('0.0.0.0', 8000): address already in use", "0.0.0.0", "8000"},
		"python.externally-managed":        {"error: externally-managed-environment"},
		"python.msvc-required":             {"error: Microsoft Visual C++ 14.0 or greater is required. Get it with \"Microsoft C++ Build Tools\""},
		"python.python2-code":              {"SyntaxError: Missing parentheses in call to 'print'. Did you mean print(...)?", "print"},
		"python.console-encoding":          {"UnicodeEncodeError: 'gbk' codec can't encode character '\\u2713' in position 0", "gbk"},
		"python.poetry-lock-outdated":      {"pyproject.toml changed significantly since poetry.lock was last generated. Run `poetry lock` to fix the lock file."},
	}
	for id, sample := range lines {
		re := rules[id]
		if re == nil {
			t.Errorf("rule %s missing", id)
			continue
		}
		if !strings.Contains(sample[0], hints[id]) {
			t.Errorf("%s: hint %q not in line", id, hints[id])
		}
		mm := re.FindStringSubmatch(sample[0])
		if mm == nil {
			t.Errorf("%s: no match", id)
			continue
		}
		for i, want := range sample[1:] {
			if mm[i+1] != want {
				t.Errorf("%s: group %d = %q, want %q", id, i+1, mm[i+1], want)
			}
		}
	}
	// 每条规则的标题/修复里引用的 %key% 都得在两份 l10n 里
	for _, loc := range []string{"zh-Hans", "en"} {
		var l10n map[string]string
		data, _ := os.ReadFile("../l10n/" + loc + ".json")
		if err := json.Unmarshal(data, &l10n); err != nil {
			t.Fatal(err)
		}
		for _, k := range regexp.MustCompile(`%([\w.]+)%`).FindAllStringSubmatch(string(b), -1) {
			if _, ok := l10n[k[1]]; !ok {
				t.Errorf("%s: missing l10n key %s", loc, k[1])
			}
		}
	}
}
