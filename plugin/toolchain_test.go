package python

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakePython 在 prefix 下放一个假的解释器文件。
func fakePython(t *testing.T, prefix string) {
	t.Helper()
	bin := filepath.Join(prefix, "bin")
	name := "python3"
	if runtime.GOOS == "windows" {
		bin, name = prefix, "python.exe"
	}
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755)
}

func TestToolchainVersionFromFiles(t *testing.T) {
	root := t.TempDir()
	uvDir := filepath.Join(root, "cpython-3.11.9-macos-aarch64-none")
	fakePython(t, uvDir)
	conda := filepath.Join(root, "conda-env")
	fakePython(t, conda)
	os.MkdirAll(filepath.Join(conda, "conda-meta"), 0o755)
	os.WriteFile(filepath.Join(conda, "conda-meta", "python-3.10.14-h2469fbe_0_cpython.json"), []byte("{}"), 0o644)
	org := filepath.Join(root, "3.12")
	fakePython(t, org)
	os.MkdirAll(filepath.Join(org, "include", "python3.12"), 0o755)
	os.WriteFile(filepath.Join(org, "include", "python3.12", "patchlevel.h"), []byte("#define PY_VERSION              \"3.12.7\"\n"), 0o644)
	venv := filepath.Join(root, "venv")
	fakePython(t, venv)
	os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte("home = /x\n"), 0o644)

	want := map[string]string{uvDir: "3.11.9", conda: "3.10.14", org: "3.12.7"}
	for _, c := range []candidate{{prefix: uvDir, source: srcUV}, {prefix: conda, source: srcConda}, {prefix: org, source: srcPythonOrg}} {
		tc, ok := inspectCandidate(nil, c)
		if !ok || tc.Version != want[c.prefix] {
			t.Errorf("%s: ok=%v version=%q", filepath.Base(c.prefix), ok, tc.Version)
		}
		if len(tc.PathPrepend) == 0 || tc.Executable == "" || tc.Label == "" {
			t.Errorf("%s: incomplete toolchain %+v", filepath.Base(c.prefix), tc)
		}
	}
	if _, ok := inspectCandidate(nil, candidate{prefix: venv, source: srcSystem}); ok {
		t.Error("a virtual environment must not be reported as a toolchain")
	}
	// 刚装好的 uv 安装目录：下面一层是 cpython-*
	got := candidatesAt(root, srcManaged)
	if len(got) < 3 {
		t.Errorf("candidatesAt = %v", got)
	}
}

func TestUVWheelTag(t *testing.T) {
	if _, ok := uvWheelTag(); !ok {
		t.Skip("platform without uv wheel")
	}
	page := `<a href="../../packages/aa/bb/uv-0.9.10-py3-none-macosx_11_0_arm64.whl#sha256=` + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" + `">x</a>`
	m := wheelLinkRe.FindStringSubmatch(page)
	if m == nil || m[3] != "0.9.10" || m[5] == "" {
		t.Fatalf("wheel link = %v", m)
	}
}
