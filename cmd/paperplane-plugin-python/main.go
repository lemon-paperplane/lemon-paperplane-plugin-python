// paperplane-plugin-python 是 Python 插件的独立可执行入口。
package main

import (
	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
	python "github.com/lemon-paperplane/lemon-paperplane-plugin-python/plugin"
)

func main() { sdk.Serve(python.New()) }
