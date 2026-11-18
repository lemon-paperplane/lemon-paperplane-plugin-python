package python

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// requirement 是一条依赖声明（PEP 508 的实用子集）。
type requirement struct {
	Name   string // 规范化后的分发包名
	Spec   string // 原始写法（去掉注释后的整行），可直接写进临时 requirements 文件
	Marker string // 环境标记，空表示无条件
	Local  bool   // 本地路径 / 可编辑安装，不参与「是否已安装」检查
}

var (
	reqNameRe  = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)`)
	eggRe      = regexp.MustCompile(`#egg=([A-Za-z0-9][A-Za-z0-9._-]*)`)
	normNameRe = regexp.MustCompile(`[-_.]+`)
)

// normName 按 PEP 503 规范化包名：小写，连续的 - _ . 换成一个 -。
func normName(s string) string {
	return normNameRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
}

// parseRequirement 解析一条 PEP 508 依赖字符串，如 "uvicorn[standard]>=0.30; python_version<'3.13'"。
func parseRequirement(s string) (requirement, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return requirement{}, false
	}
	r := requirement{Spec: s}
	body := s
	if i := strings.Index(s, ";"); i >= 0 {
		body = strings.TrimSpace(s[:i])
		r.Marker = strings.TrimSpace(s[i+1:])
	}
	// URL / 本地路径形式
	if strings.Contains(body, "://") || strings.HasPrefix(body, ".") || strings.HasPrefix(body, "/") || strings.HasPrefix(body, "file:") {
		if !strings.Contains(body, " @ ") && !strings.Contains(body, "@ ") {
			if m := eggRe.FindStringSubmatch(body); m != nil {
				r.Name = normName(m[1])
			}
			r.Local = true
			return r, r.Name != "" || r.Local
		}
	}
	m := reqNameRe.FindStringSubmatch(body)
	if m == nil {
		return requirement{}, false
	}
	r.Name = normName(m[1])
	if at := strings.Index(body, "@"); at >= 0 && strings.Contains(body[at:], "://") {
		rest := strings.TrimSpace(body[at+1:])
		if strings.HasPrefix(rest, "file:") {
			r.Local = true
		}
	}
	return r, true
}

// parseRequirementsFile 读取 requirements 文件（跟随 -r 引用，最多 5 层）。
func parseRequirementsFile(path string) []requirement {
	return parseReqFile(path, map[string]bool{}, 0)
}

func parseReqFile(path string, seen map[string]bool, depth int) []requirement {
	abs, _ := filepath.Abs(path)
	if seen[abs] || depth > 5 {
		return nil
	}
	seen[abs] = true
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []requirement
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var cont strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if strings.HasSuffix(line, "\\") {
			cont.WriteString(strings.TrimSuffix(line, "\\"))
			cont.WriteByte(' ')
			continue
		}
		if cont.Len() > 0 {
			cont.WriteString(line)
			line = cont.String()
			cont.Reset()
		}
		line = stripReqComment(line)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "-r ") || strings.HasPrefix(line, "--requirement"):
			ref := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "--requirement"), "-r"))
			ref = strings.TrimPrefix(ref, "=")
			if ref != "" {
				out = append(out, parseReqFile(filepath.Join(filepath.Dir(path), ref), seen, depth+1)...)
			}
		case strings.HasPrefix(line, "-e ") || strings.HasPrefix(line, "--editable"):
			ref := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "--editable"), "-e"))
			r := requirement{Spec: line, Local: true}
			if m := eggRe.FindStringSubmatch(ref); m != nil {
				r.Name = normName(m[1])
			}
			out = append(out, r)
		case strings.HasPrefix(line, "-"):
			// -c / -i / --index-url / --extra-index-url / --find-links 等选项，不是依赖
		default:
			if r, ok := parseRequirement(line); ok {
				out = append(out, r)
			}
		}
	}
	return out
}

// stripReqComment 去掉 requirements 行内注释（# 前须是空白或行首，URL 里的 #egg= 不算）。
func stripReqComment(line string) string {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "#") {
		return ""
	}
	for i := 1; i < len(line); i++ {
		if line[i] == '#' && (line[i-1] == ' ' || line[i-1] == '\t') {
			return strings.TrimSpace(line[:i])
		}
	}
	return line
}

// ---------------------------------------------------------------------------
// 环境标记（PEP 508 markers）求值：支持常见变量、比较运算、in / not in、and / or、括号。
// 无法解析时返回 true（宁可多算一个依赖，也不漏）。

type markerEnv map[string]string

func evalMarker(marker string, env markerEnv) bool {
	marker = strings.TrimSpace(marker)
	if marker == "" {
		return true
	}
	p := &markerParser{toks: tokenizeMarker(marker), env: env}
	v, ok := p.parseOr()
	if !ok || p.pos != len(p.toks) {
		return true
	}
	return v
}

type markerParser struct {
	toks []string
	pos  int
	env  markerEnv
}

func tokenizeMarker(s string) []string {
	var out []string
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '(' || c == ')':
			out = append(out, string(c))
			i++
		case c == '"' || c == '\'':
			j := strings.IndexByte(s[i+1:], c)
			if j < 0 {
				out = append(out, s[i:])
				return out
			}
			out = append(out, s[i:i+j+2])
			i += j + 2
		case strings.ContainsRune("<>=!~", rune(c)):
			j := i
			for j < len(s) && strings.ContainsRune("<>=!~", rune(s[j])) {
				j++
			}
			out = append(out, s[i:j])
			i = j
		default:
			j := i
			for j < len(s) && !strings.ContainsRune(" \t()<>=!~\"'", rune(s[j])) {
				j++
			}
			out = append(out, s[i:j])
			i = j
		}
	}
	return out
}

func (p *markerParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *markerParser) parseOr() (bool, bool) {
	v, ok := p.parseAnd()
	if !ok {
		return false, false
	}
	for p.peek() == "or" {
		p.pos++
		r, ok := p.parseAnd()
		if !ok {
			return false, false
		}
		v = v || r
	}
	return v, true
}

func (p *markerParser) parseAnd() (bool, bool) {
	v, ok := p.parseAtom()
	if !ok {
		return false, false
	}
	for p.peek() == "and" {
		p.pos++
		r, ok := p.parseAtom()
		if !ok {
			return false, false
		}
		v = v && r
	}
	return v, true
}

func (p *markerParser) parseAtom() (bool, bool) {
	if p.peek() == "(" {
		p.pos++
		v, ok := p.parseOr()
		if !ok || p.peek() != ")" {
			return false, false
		}
		p.pos++
		return v, true
	}
	if p.pos+1 >= len(p.toks) {
		return false, false
	}
	left := p.toks[p.pos]
	op := p.toks[p.pos+1]
	p.pos += 2
	if op == "not" && p.peek() == "in" {
		op = "not in"
		p.pos++
	}
	if p.pos >= len(p.toks) {
		return false, false
	}
	right := p.toks[p.pos]
	p.pos++
	lv, lq, lok := p.value(left)
	rv, rq, rok := p.value(right)
	if !lok || !rok {
		return false, false
	}
	// 变量 extra 永远视为不满足（我们不装可选依赖）
	if left == "extra" || right == "extra" {
		return false, true
	}
	return compareMarker(lv, op, rv, lq && rq || isVersionVar(left) || isVersionVar(right)), true
}

func isVersionVar(name string) bool {
	return name == "python_version" || name == "python_full_version" || name == "implementation_version"
}

// value 返回标记里一个操作数的值；quoted 表示是字面量。
func (p *markerParser) value(tok string) (string, bool, bool) {
	if len(tok) >= 2 && (tok[0] == '"' || tok[0] == '\'') {
		return tok[1 : len(tok)-1], true, true
	}
	if tok == "extra" {
		return "", false, true
	}
	v, ok := p.env[tok]
	return v, false, ok
}

func compareMarker(l, op, r string, version bool) bool {
	switch op {
	case "in":
		return strings.Contains(r, l)
	case "not in":
		return !strings.Contains(r, l)
	}
	if version {
		if _, ok := parseLooseVersion(l); ok {
			if _, ok2 := parseLooseVersion(r); ok2 {
				c := compareLoose(l, r)
				switch op {
				case "==", "===":
					return c == 0 || (strings.HasSuffix(r, ".*") && strings.HasPrefix(l, strings.TrimSuffix(r, ".*")))
				case "!=":
					return c != 0
				case "<":
					return c < 0
				case "<=":
					return c <= 0
				case ">":
					return c > 0
				case ">=", "~=":
					return c >= 0
				}
			}
		}
	}
	switch op {
	case "==", "===":
		return l == r
	case "!=":
		return l != r
	case "<":
		return l < r
	case "<=":
		return l <= r
	case ">":
		return l > r
	case ">=":
		return l >= r
	}
	return true
}

func parseLooseVersion(s string) (sdk.Version, bool) {
	return sdk.ParseVersion(strings.TrimSuffix(s, ".*"))
}

func compareLoose(a, b string) int {
	return sdk.CompareVersions(strings.TrimSuffix(a, ".*"), strings.TrimSuffix(b, ".*"))
}
