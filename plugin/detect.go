package python

import (
	"path/filepath"
	"sort"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// 嵌套在 Python 工程里时，这些目录只有 requirements 也不单独成模块。
var auxDirs = map[string]bool{"docs": true, "doc": true, "documentation": true, "tests": true, "test": true, "testing": true, "requirements": true, "ci": true, ".github": true, "scripts": true, "tools": true, "benchmarks": true}

// 第三方代码目录：里面的 setup.py / pyproject 是被打包进来的依赖，不是独立工程。
var vendorDirs = map[string]bool{"vendor": true, "vendored": true, "third_party": true, "third-party": true, "thirdparty": true, "3rdparty": true, "external": true, "externals": true, "site-packages": true, "node_modules": true}

// Detect 只看文件，判断目录是不是 Python 模块。
func (p *Plugin) Detect(ctx *sdk.Context, d *sdk.DetectParams) (*sdk.DetectResult, error) {
	pf := readProject(d.Dir, d.Files)
	if !shouldDetect(d.Root, d.Dir, d.RelDir, pf) {
		return &sdk.DetectResult{}, nil
	}
	mgr := pf.manager()
	data := map[string]any{"manager": mgr}
	var deps []string
	for _, r := range pf.declaredDeps() {
		if r.Name != "" {
			deps = append(deps, r.Name)
		}
		if len(deps) >= 300 {
			break
		}
	}
	if len(deps) > 0 {
		data["deps"] = deps
	}
	res := &sdk.DetectResult{Facets: []sdk.Facet{{ID: "python", Data: data}}, Name: pf.name()}
	switch mgr {
	case mgrUV, mgrPoetry, mgrPDM, mgrPipenv, mgrConda:
		res.Facets = append(res.Facets, sdk.Facet{ID: mgr})
	}
	if pf.strongMarker() || len(pf.Requirements) > 0 {
		res.Claims = projectClaims(pf)
	}
	return res, nil
}

// shouldDetect 决定目录是否成为独立的 Python 模块。
func shouldDetect(root, dir, relDir string, pf *projectFiles) bool {
	strong := pf.strongMarker()
	reqs := len(pf.Requirements) > 0
	for _, seg := range strings.Split(filepath.ToSlash(relDir), "/") {
		if vendorDirs[strings.ToLower(seg)] {
			return false
		}
	}
	nested := relDir != "" && insidePythonProject(root, dir)
	if nested {
		// 工程里的子目录：有自己的 pyproject / setup.py / Pipfile 才是独立工程；
		// 只有 requirements 的，要有 .py 且不是 docs/tests 之类的辅助目录。
		if strong {
			return true
		}
		return reqs && pf.hasPyFiles() && !auxDirs[strings.ToLower(filepath.Base(dir))]
	}
	if strong || reqs || pf.ManagePy {
		if pf.Pyproject != nil && !pf.isPythonPyproject() && !reqs && !pf.ManagePy && !pf.hasPyFiles() {
			return false
		}
		return true
	}
	// 没有任何声明文件：只在工程根与第一层子目录里认带 __main__ 入口的脚本
	if strings.Count(filepath.ToSlash(relDir), "/") >= 1 {
		return false
	}
	return len(mainScripts(dir, pf.Files, 1)) > 0
}

// insidePythonProject 判断 dir 的某个上级（直到工程根）是不是 Python 工程。
func insidePythonProject(root, dir string) bool {
	root = filepath.Clean(root)
	for cur := filepath.Dir(filepath.Clean(dir)); ; cur = filepath.Dir(cur) {
		if len(cur) < len(root) {
			return false
		}
		pf := readProject(cur, nil)
		if pf.strongMarker() || len(pf.Requirements) > 0 {
			return true
		}
		if cur == root || cur == filepath.Dir(cur) {
			return false
		}
	}
}

// 其他语言/构建系统的工程标记：子目录里有这些就是独立的非 Python 工程（如前端），不认领也不往下走。
var foreignMarkers = []string{"package.json", "go.mod", "go.work", "pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts", "Cargo.toml", "composer.json", "Gemfile", "pubspec.yaml", "*.csproj", "*.sln", "CMakeLists.txt", "Package.swift", "*.xcodeproj/"}

// projectClaims 列出属于本 Python 工程的子目录（相对 pf.Dir），内核不再把它们识别成独立模块：
//   - 第一层的 Python 代码目录（包、src、tests 等）；
//   - 子树里自带 setup.py / pyproject 的内嵌库：在 vendor/third_party 下、是 docs/tests 之类的辅助目录、
//     或被本工程引用（-e 路径依赖、uv workspace 成员、uv/poetry 的 path 源）。
//
// 带其他语言工程标记的目录（前端、Go…）不认领，保留为独立模块；不用 "**"。
func projectClaims(pf *projectFiles) []string {
	refs := pf.referencedPaths()
	var claims []string
	visited := 0
	var walk func(dir, rel string, depth int, underVendor bool)
	walk = func(dir, rel string, depth int, underVendor bool) {
		for _, name := range listDir(dir) {
			if !strings.HasSuffix(name, "/") || visited > 3000 || len(claims) >= 200 {
				continue
			}
			base := strings.TrimSuffix(name, "/")
			if sdk.DefaultSkipDirs[base] || strings.HasPrefix(base, ".") {
				continue
			}
			visited++
			sub := filepath.Join(dir, base)
			subRel := base
			if rel != "" {
				subRel = rel + "/" + base
			}
			files := listDir(sub)
			if sdk.HasFile(files, foreignMarkers...) {
				continue // 独立的非 Python 工程
			}
			vendor := underVendor || vendorDirs[strings.ToLower(base)]
			child := &projectFiles{Dir: sub, Files: files}
			child.Requirements = requirementFiles(sub, files)
			hasMarkers := sdk.HasFile(files, "pyproject.toml", "setup.py", "setup.cfg", "Pipfile", "environment.yml", "environment.yaml") || len(child.Requirements) > 0
			switch {
			case depth == 1 && !hasMarkers && (child.hasPyFiles() || auxDirs[strings.ToLower(base)]):
				claims = append(claims, subRel)
			case hasMarkers && (vendor || auxDirs[strings.ToLower(base)] || refs.match(subRel)):
				claims = append(claims, subRel)
			case hasMarkers:
				continue // 独立的嵌套 Python 工程：保留为单独模块，也不再往里认领
			}
			if depth < 4 {
				walk(sub, subRel, depth+1, vendor)
			}
		}
	}
	walk(pf.Dir, "", 1, false)
	sort.Strings(claims)
	return claims
}

// pathRefs 是工程引用的本地路径（相对工程根，可含 glob）。
type pathRefs []string

func (r pathRefs) match(rel string) bool {
	for _, p := range r {
		if p == rel {
			return true
		}
		if ok, _ := filepath.Match(p, rel); ok {
			return true
		}
	}
	return false
}

// referencedPaths 收集 requirements 里的 -e/本地路径、uv workspace 成员、uv sources 与 poetry 的 path 依赖。
func (pf *projectFiles) referencedPaths() pathRefs {
	var out pathRefs
	add := func(p string) {
		p = strings.TrimSpace(p)
		p = strings.TrimPrefix(p, "file:")
		p = strings.TrimPrefix(filepath.ToSlash(p), "./")
		p = strings.TrimSuffix(p, "/")
		if p != "" && p != "." && !filepath.IsAbs(p) && !strings.Contains(p, "://") {
			out = append(out, p)
		}
	}
	for _, f := range pf.Requirements {
		for _, r := range parseRequirementsFile(filepath.Join(pf.Dir, f)) {
			if !r.Local {
				continue
			}
			spec := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(r.Spec, "--editable"), "-e"))
			spec = strings.SplitN(spec, "#", 2)[0]
			spec = strings.SplitN(spec, "[", 2)[0]
			add(spec)
		}
	}
	if pp := pf.Pyproject; pp != nil {
		if ws, ok := pp.Tool.UV["workspace"].(map[string]any); ok {
			if members, ok := ws["members"].([]any); ok {
				for _, m := range members {
					add(toString(m))
				}
			}
		}
		if src, ok := pp.Tool.UV["sources"].(map[string]any); ok {
			for _, v := range src {
				if t, ok := v.(map[string]any); ok {
					if p, ok := t["path"].(string); ok {
						add(p)
					}
				}
			}
		}
		if pp.Tool.Poetry != nil {
			for _, v := range pp.Tool.Poetry.Dependencies {
				if t, ok := v.(map[string]any); ok {
					if p, ok := t["path"].(string); ok {
						add(p)
					}
				}
			}
		}
	}
	return out
}
