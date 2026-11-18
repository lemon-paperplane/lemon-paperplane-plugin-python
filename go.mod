module github.com/lemon-paperplane/lemon-paperplane-plugin-python

go 1.23

require (
	github.com/BurntSushi/toml v1.5.0
	github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk v0.0.0
)

replace github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk => ../lemon-paperplane-main/lemon-paperplane-main-plugin-sdk
