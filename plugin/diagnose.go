package python

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

var (
	uvPyNotSatisfyRe = regexp.MustCompile(`(?:current|requested) Python version \(([\d.]+)\) does not satisfy Python\s*([<>=!~][^ ,]*(?:,\s*[<>=!~][^ ,]*)*)`)
	abiTagsRe        = regexp.MustCompile("following Python ABI tags?: ((?:`cp\\d+[a-z]*`(?:, | and )?)+)")
	cpTagRe          = regexp.MustCompile("`cp3(\\d+)")
	usingPythonRe    = regexp.MustCompile(`Using (?:CPython |Python )?(\d+\.\d+)\.\d+`)
	noDistRe         = regexp.MustCompile(`No matching distribution found for|Could not find a version that satisfies the requirement|no wheels with a matching Python`)
	addrInUseRe      = regexp.MustCompile(`\[Errno (?:48|98)\] Address already in use|\[WinError 10048\]`)
	portInLogRe      = regexp.MustCompile(`(?:port[ =:]+|:)(\d{4,5})\b`)
	moduleMissingRe  = regexp.MustCompile(`ModuleNotFoundError: No module named '([A-Za-z0-9_]+)`)
)

// Diagnose 补充声明式规则做不到的诊断：从 uv/pip 的报错推断该换哪个 Python 版本；端口占用补端口号；本地模块导入失败。
func (p *Plugin) Diagnose(ctx *sdk.Context, in *sdk.DiagnoseParams) (*sdk.DiagnoseResult, error) {
	var out []sdk.Diagnosis
	matched := map[string]bool{}
	for _, d := range in.Matched {
		matched[d.RuleID] = true
	}
	text := strings.Join(in.LogTail, "\n")

	// 1. 依赖要求的 Python 版本与当前不符
	if !matched["python.requires-different-python"] && !matched["python.uv-python-unsatisfied"] {
		if c := suggestPython(text); c != "" {
			out = append(out, sdk.Diagnosis{
				RuleID: "python.suggest-python", Severity: "error",
				Title:   ctx.T("diag.suggestPython.title"),
				Message: ctx.T("diag.suggestPython.message", c),
				Fixes:   []sdk.Fix{{ID: "python", Title: ctx.T("fix.usePython", c), Kind: sdk.FixSetToolchain, Params: map[string]string{"kind": "python", "constraint": c}, Recommended: true}},
			})
		}
	}

	// 2. 端口被占用但日志里没有端口号：用运行配置的 port 选项或日志里最近出现的端口
	// 带 port 选项的运行配置（django / fastapi / flask）由内核按 LaunchSpec.Ports 检测、框架插件给换端口修复，这里只管脚本
	hasPortOpt := in.Config != nil && in.Config.Options["port"] != nil
	if addrInUseRe.MatchString(text) && !matched["python.uvicorn-address-in-use"] && !hasPortOpt {
		port := ""
		if port == "" {
			for i := len(in.LogTail) - 1; i >= 0 && port == ""; i-- {
				if m := portInLogRe.FindStringSubmatch(in.LogTail[i]); m != nil {
					port = m[1]
				}
			}
		}
		if port != "" {
			out = append(out, sdk.Diagnosis{
				RuleID: "python.port-in-use", Severity: "error",
				Title: ctx.T("diag.portInUse.title", port),
				Fixes: []sdk.Fix{{ID: "kill", Title: ctx.T("fix.killPort", port), Kind: sdk.FixKillPort, Params: map[string]string{"port": port}, Recommended: true}},
			})
		} else {
			out = append(out, sdk.Diagnosis{RuleID: "python.port-in-use", Severity: "error", Title: ctx.T("diag.addressInUse.title")})
		}
	}

	// 3. 导入工程自己的模块失败：多半是工作目录 / 源码根不对，而不是缺包
	if m := moduleMissingRe.FindStringSubmatch(text); m != nil && in.Module.Dir != "" {
		if loc := localModule(in.Module.Dir, m[1]); loc != "" {
			out = append(out, sdk.Diagnosis{
				RuleID: "python.local-module", Severity: "error",
				Title:   ctx.T("diag.localModule.title", m[1]),
				Message: ctx.T("diag.localModule.message", loc),
				Fixes: []sdk.Fix{{ID: "wd", Title: ctx.T("fix.setWorkingDir", filepath.Dir(loc)), Kind: sdk.FixSetRunOption,
					Params: map[string]string{"key": "workingDir", "value": filepath.ToSlash(filepath.Dir(loc))}}},
			})
		}
	}
	return &sdk.DiagnoseResult{Diagnoses: out}, nil
}

// suggestPython 从安装输出推断应改用的 Python 版本约束。
func suggestPython(text string) string {
	if m := uvPyNotSatisfyRe.FindStringSubmatch(text); m != nil {
		return cleanPyConstraint(m[2])
	}
	if m := abiTagsRe.FindStringSubmatch(text); m != nil {
		best := -1
		for _, t := range cpTagRe.FindAllStringSubmatch(m[1], -1) {
			if n, _ := strconv.Atoi(t[1]); n > best {
				best = n
			}
		}
		if best >= 0 {
			return "3." + strconv.Itoa(best)
		}
	}
	if noDistRe.MatchString(text) {
		if m := usingPythonRe.FindStringSubmatch(text); m != nil {
			parts := strings.Split(m[1], ".")
			if minor, err := strconv.Atoi(parts[1]); err == nil && minor > 8 {
				return "3." + strconv.Itoa(minor-1)
			}
		}
	}
	return ""
}

// localModule 判断名字是不是工程里自己的模块（目录或 .py 文件），返回其相对路径。
func localModule(dir, name string) string {
	var found []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		relp, _ := filepath.Rel(dir, path)
		depth := strings.Count(relp, string(filepath.Separator))
		if d.IsDir() {
			if path != dir && (sdk.DefaultSkipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") || depth >= 3) {
				return filepath.SkipDir
			}
			if d.Name() == name && path != dir {
				found = append(found, relp)
			}
			return nil
		}
		if d.Name() == name+".py" {
			found = append(found, relp)
		}
		return nil
	})
	if len(found) == 0 {
		return ""
	}
	sort.Slice(found, func(i, j int) bool { return len(found[i]) < len(found[j]) })
	// 就在模块根下的话不是工作目录的问题
	if !strings.Contains(found[0], string(filepath.Separator)) {
		return ""
	}
	return found[0]
}

// 常见「导入名 ≠ 包名」的对照表。
var importToPackage = map[string]string{
	"cv2": "opencv-python", "PIL": "pillow", "yaml": "pyyaml", "sklearn": "scikit-learn", "skimage": "scikit-image",
	"bs4": "beautifulsoup4", "dotenv": "python-dotenv", "jwt": "pyjwt", "Crypto": "pycryptodome", "docx": "python-docx",
	"pptx": "python-pptx", "magic": "python-magic", "attr": "attrs", "dateutil": "python-dateutil", "serial": "pyserial",
	"usb": "pyusb", "OpenSSL": "pyopenssl", "MySQLdb": "mysqlclient", "psycopg2": "psycopg2-binary", "fitz": "pymupdf",
	"win32api": "pywin32", "win32con": "pywin32", "win32com": "pywin32", "gi": "pygobject", "wx": "wxpython",
	"zmq": "pyzmq", "Levenshtein": "python-levenshtein", "telegram": "python-telegram-bot", "kafka": "kafka-python",
	"jose": "python-jose", "multipart": "python-multipart", "socketio": "python-socketio", "engineio": "python-engineio",
	"git": "gitpython", "cairo": "pycairo", "ldap": "python-ldap", "Bio": "biopython", "google.protobuf": "protobuf",
	"pkg_resources": "setuptools", "distutils": "setuptools", "markdown": "markdown", "lxml": "lxml", "pymysql": "pymysql",
	"redis": "redis", "jinja2": "jinja2", "Levenshtein_": "levenshtein", "mpl_toolkits": "matplotlib", "nacl": "pynacl",
	"pythoncom": "pywin32", "speech_recognition": "SpeechRecognition", "slugify": "python-slugify", "consul": "python-consul",
	"snappy": "python-snappy", "jieba": "jieba", "pypinyin": "pypinyin", "ruamel": "ruamel.yaml", "tensorflow": "tensorflow",
	"torch": "torch", "paddle": "paddlepaddle", "pyecharts": "pyecharts", "qrcode": "qrcode", "Xlib": "python-xlib",
}

// packageForImport 把导入名映射成 PyPI 包名。
func packageForImport(name string) string {
	if p, ok := importToPackage[name]; ok {
		return p
	}
	return strings.ReplaceAll(name, "_", "-")
}
