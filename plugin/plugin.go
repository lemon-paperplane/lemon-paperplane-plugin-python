// Package python 是 Lemon Paperplane 的 Python 插件：发现与安装 Python 工具链（uv/pyenv/conda/Homebrew/python.org/系统），
// 识别 Python 工程与包管理器（uv/Poetry/PDM/Pipenv/conda/pip），管理工程虚拟环境、安装依赖、运行脚本与诊断常见报错。
//
// 依赖它的框架插件（django / fastapi / flask）通过服务 resolveEnv 拿到解释器与运行环境。
package python

import "sync"

// Plugin 实现 Detector、Analyzer、DependencyInstaller、RunPreparer、Diagnoser、FixApplier、ActionInvoker、
// ToolchainProvider、ServiceProvider、ViewProvider、ToolProvider。
type Plugin struct {
	uvMu sync.Mutex
	uv   string // 已找到/已下载的 uv
}

// New 创建插件实例。
func New() any { return &Plugin{} }
