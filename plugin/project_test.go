package python

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func names(reqs []requirement) []string {
	var out []string
	for _, r := range reqs {
		if r.Name != "" {
			out = append(out, r.Name)
		}
	}
	sort.Strings(out)
	return out
}

func TestParseRequirement(t *testing.T) {
	cases := []struct {
		in, name, marker string
		local            bool
	}{
		{"Django>=4.2,<5", "django", "", false},
		{"uvicorn[standard]>=0.30", "uvicorn", "", false},
		{"pywin32; sys_platform == 'win32'", "pywin32", "sys_platform == 'win32'", false},
		{"Some_Pkg.Name==1.0", "some-pkg-name", "", false},
		{"lightglue @ git+https://github.com/cvg/LightGlue.git", "lightglue", "", false},
		{"git+https://github.com/org/repo.git#egg=Some_Pkg", "some-pkg", "", true},
		{"./libs/shared", "", "", true},
	}
	for _, c := range cases {
		r, ok := parseRequirement(c.in)
		if !ok {
			t.Fatalf("%q: not parsed", c.in)
		}
		if r.Name != c.name || r.Marker != c.marker || r.Local != c.local {
			t.Errorf("%q: got name=%q marker=%q local=%v", c.in, r.Name, r.Marker, r.Local)
		}
	}
}

func TestRequirementsFileIncludesAndOptions(t *testing.T) {
	reqs := parseRequirementsFile("testdata/reqproj/requirements.txt")
	got := names(reqs)
	want := []string{"django", "numpy", "psycopg", "pyobjc-core", "pywin32", "requests", "some-pkg"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	locals := 0
	for _, r := range reqs {
		if r.Local {
			locals++
		}
	}
	if locals != 2 { // -e ./libs/shared 与 git+…#egg=
		t.Errorf("local = %d, want 2", locals)
	}
}

func TestMarkers(t *testing.T) {
	mac := markerEnv{"sys_platform": "darwin", "platform_system": "Darwin", "os_name": "posix", "python_version": "3.12", "python_full_version": "3.12.4", "platform_machine": "arm64"}
	cases := map[string]bool{
		``:                            true,
		`sys_platform == 'win32'`:     false,
		`platform_system == "Darwin"`: true,
		`python_version >= "3.9"`:     true,
		`python_version < "3.10"`:     false,
		`python_version >= "3.11" and python_version < "4.0"`:                      true,
		`(os_name == "nt" or sys_platform == "darwin") and python_version > "3.8"`: true,
		`extra == "dev"`:                     false,
		`platform_machine in "x86_64 AMD64"`: false,
		`python_full_version >= "3.12.1"`:    true,
		`this is not a marker`:               true,
	}
	for m, want := range cases {
		if got := evalMarker(m, mac); got != want {
			t.Errorf("evalMarker(%q) = %v, want %v", m, got, want)
		}
	}
}

func TestManagerAndRequirements(t *testing.T) {
	cases := []struct {
		dir, manager, constraint, preferred, name string
		deps                                      []string
	}{
		{"uvproj", mgrUV, ">=3.12,<3.13", "3.12", "uv-proj", []string{"fastapi", "pywin32", "uvicorn"}},
		{"poetryproj", mgrPoetry, ">=3.10,<4", "", "Poetry Proj", []string{"colorama", "django", "local-lib", "requests"}},
		{"pipenvproj", mgrPipenv, "3.11", "", "", []string{"flask", "gunicorn"}},
		{"condaproj", mgrConda, "3.10", "", "", []string{"requests", "rich"}},
		{"reqproj", mgrPip, "3.11", "3.11.4", "", []string{"django", "numpy", "psycopg", "pyobjc-core", "pywin32", "requests", "some-pkg"}},
		{"setupproj", mgrPip, ">=3.8", "", "setup-proj", []string{"click", "pyyaml"}},
		{"loose", mgrNone, "", "", "", nil},
	}
	for _, c := range cases {
		pf := readProject(filepath.Join("testdata", c.dir), nil)
		if got := pf.manager(); got != c.manager {
			t.Errorf("%s: manager = %q, want %q", c.dir, got, c.manager)
		}
		r := pf.pythonRequirement()
		if r.Constraint != c.constraint || r.Preferred != c.preferred {
			t.Errorf("%s: python = %q / %q, want %q / %q", c.dir, r.Constraint, r.Preferred, c.constraint, c.preferred)
		}
		if got := pf.name(); got != c.name {
			t.Errorf("%s: name = %q, want %q", c.dir, got, c.name)
		}
		if got := names(pf.declaredDeps()); !reflect.DeepEqual(got, c.deps) {
			t.Errorf("%s: deps = %v, want %v", c.dir, got, c.deps)
		}
	}
}

func TestRequirementFilesOrder(t *testing.T) {
	pf := readProject("testdata/reqproj", nil)
	want := []string{"requirements.txt", "requirements-dev.txt"}
	if !reflect.DeepEqual(pf.Requirements, want) {
		t.Fatalf("requirements = %v, want %v", pf.Requirements, want)
	}
	files := []string{"requirements/", "manage.py"}
	got := requirementFiles("testdata/reqproj", files)
	if len(got) == 0 || got[0] != "requirements/base.txt" {
		t.Fatalf("requirements/ dir: %v", got)
	}
}

func TestPoetryConversion(t *testing.T) {
	pf := readProject("testdata/poetryproj", nil)
	reqs := poetryRequirements(pf.Pyproject.Tool.Poetry.Dependencies)
	specs := map[string]string{}
	for _, r := range reqs {
		specs[r.Name] = r.Spec
	}
	if specs["requests"] != "requests>=2.31,<3" {
		t.Errorf("requests spec = %q", specs["requests"])
	}
	if specs["django"] != "django[argon2]>=4.2,<4.3" {
		t.Errorf("django spec = %q", specs["django"])
	}
	if !strings.Contains(specs["colorama"], "; sys_platform == 'win32'") {
		t.Errorf("colorama spec = %q", specs["colorama"])
	}
	if _, ok := specs["optional-one"]; ok {
		t.Error("optional dependency should be skipped")
	}
	for in, want := range map[string]string{"^1.2.3": ">=1.2.3,<2", "^0.4": ">=0.4,<0.5", "~1.2": ">=1.2,<1.3", "1.0.0": "==1.0.0", "*": ""} {
		if got := poetryVersion(in); got != want {
			t.Errorf("poetryVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPipfileLock(t *testing.T) {
	reqs := pipfileLockRequirements("testdata/pipenvproj/Pipfile.lock")
	var specs []string
	for _, r := range reqs {
		specs = append(specs, r.Spec)
	}
	want := []string{"flask==3.0.3", "gunicorn==22.0.0; sys_platform != 'win32'", "pytest==8.2.0"}
	if !reflect.DeepEqual(specs, want) {
		t.Fatalf("specs = %v, want %v", specs, want)
	}
}

func TestEnvYml(t *testing.T) {
	name, _ := envYmlInfo("testdata/condaproj/environment.yml")
	if name != "ml-env" {
		t.Errorf("name = %q", name)
	}
}

func TestCleanPyConstraint(t *testing.T) {
	for in, want := range map[string]string{
		">=3.8, !=3.9.0, <4": ">=3.8,<4",
		"~=3.10":             "~=3.10",
		">= 3.12":            ">=3.12",
	} {
		if got := cleanPyConstraint(in); got != want {
			t.Errorf("cleanPyConstraint(%q) = %q, want %q", in, got, want)
		}
	}
	if got := readPythonVersionFile("testdata/uvproj/.python-version"); got != "3.12" {
		t.Errorf(".python-version = %q", got)
	}
}

func TestPoetryEnvPrefix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("路径大小写规则不同")
	}
	// 实测：poetry 2.x 在该目录生成的环境名是 poetry-demo-_sRIJgK6-py3.13
	got := poetryEnvPrefix("poetry-demo", "/private/tmp/claude-501/-Users-liuri-Code/85cf8c15-6184-45f0-9da0-5eeb30773565/scratchpad/samples/poetry-demo")
	if got != "poetry-demo-_sRIJgK6-py" {
		t.Fatalf("prefix = %q", got)
	}
}

func TestDotenv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	os.WriteFile(path, []byte("# c\nexport A=1\nB='x y'\nC=\"line\\nnext\"\nD=${A}-z # tail\nE=${MISSING:-def}\nbad line\n"), 0o644)
	env := loadDotenv(path)
	want := map[string]string{"A": "1", "B": "x y", "C": "line\nnext", "D": "1-z", "E": "def"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("dotenv = %#v", env)
	}
	// .env 是目录（老式虚拟环境）时不读
	os.Remove(path)
	os.Mkdir(path, 0o755)
	if loadDotenv(path) != nil {
		t.Fatal("directory .env should be ignored")
	}
}

func TestEntryPointCode(t *testing.T) {
	if attrChain("main") != "" || attrChain("App.run") != ".run" {
		t.Fatal("attrChain")
	}
}
