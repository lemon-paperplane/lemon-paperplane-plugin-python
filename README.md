# lemon-paperplane-plugin-python

Lemon Paperplane 的 Python 插件。

- 工具链 `python`：发现 uv / pyenv / conda（base 与 envs）/ Homebrew / python.org / Xcode / 系统 / Windows py 启动器的解释器，版本从文件读取；用 uv 下载安装（`UV_PYTHON_INSTALL_MIRROR` 镜像）。本机没有 uv 时自动从 PyPI 镜像下载 uv 的 wheel 到插件数据目录。
- 识别：pyproject.toml、requirements*.txt、setup.py/cfg、Pipfile、environment.yml、manage.py、带 `__main__` 的脚本；包管理器 uv / Poetry / PDM / Pipenv / conda / pip。工程根用 Claims 认领自己的子目录（包、docs、tests、vendor 里的内嵌库），前端等其他语言子工程保留为独立模块。
- 虚拟环境：优先用工程里已有的（.venv / venv / env，Poetry 的缓存环境，conda 具名环境），坏了报问题并提供重建；没有就用 uv 在工程里建 `.venv`。
- 依赖安装不改锁文件：`uv sync --frozen --inexact`；Poetry / PDM 锁文件先导出成临时 requirements 再 `uv pip install`；Pipenv 读 Pipfile.lock；requirements / pyproject / setup.py 用 `uv pip install -r`；conda 用 `conda env create/update`。
- 运行配置 `python-script`：脚本或模块（`pkg.mod:func` 入口点）、参数、工作目录、`.env`、环境变量。首次运行自动建环境、补装依赖。
- 诊断：缺模块、Python 版本不符（从 uv/pip 报错推断版本）、端口占用、externally-managed、MSVC、distutils、编码、Poetry 锁过期等。
- 服务 `resolveEnv` / `installPackages` 供 django / fastapi / flask 插件使用；界面「包」标签页；MCP 工具 `python.packages`。

构建：`./build.sh`；测试：`go test ./...`。
