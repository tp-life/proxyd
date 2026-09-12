#!/bin/sh
# 准备带本地补丁的第三方依赖源码（mihomo 与 metacubex/tailscale）；无参数，失败返回非零。
# 依赖 Go、Git、patch 和标准 Unix 工具；仅写项目 third_party，不修改 Go module cache。
# 完整源码不纳入版本控制，版本取自 go.mod，补丁随项目发布。
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

mkdir -p "$root/third_party"
lock="$root/third_party/.prepare-lock"
# mkdir 原子抢占锁，避免多个 make 进程同时替换依赖；冲突时交由调用方重试。
mkdir "$lock" 2>/dev/null || { echo "另一个依赖准备进程正在运行，请稍后重试" >&2; exit 1; }
stage=""
trap 'test -z "$stage" || rm -rf "$stage"; rmdir "$lock"' EXIT
trap 'exit 1' HUP INT TERM

# prepare 下载固定版本并在临时目录应用补丁；失败时保留现有依赖。
# 参数：module（go.mod 中的模块路径）、patch 文件、third_party 下的目标目录。
# 相同版本和补丁重复执行不重新复制源码；升级版本或修改补丁后自动重新生成。
prepare() {
	module=$1
	patch_file="$root/$2"
	destination="$root/third_party/$3"
	version=$(sed -n "s|^[[:space:]]*$module \\(v[^[:space:]]*\\).*\$|\\1|p" go.mod | head -1)
	test -n "$version" || { echo "无法从 go.mod 读取 $module 版本" >&2; exit 1; }
	fingerprint=$({ printf '%s\n' "$version"; cat "$patch_file"; } | git hash-object --stdin)
	if [ -f "$destination/go.mod" ] && [ -f "$destination/.proxyd-patch" ] &&
	   [ "$(cat "$destination/.proxyd-patch")" = "$fingerprint" ]; then
		return 0
	fi

	# Go 使用模块校验机制下载固定版本。
	go mod download "$module@$version"
	cache=$(go env GOMODCACHE)
	stage=$(mktemp -d "$root/third_party/.prepare.XXXXXX")
	cp -R "$cache/$module@$version" "$stage/source"
	chmod -R u+w "$stage/source"
	patch -t -N -p1 -d "$stage/source" < "$patch_file"
	printf '%s\n' "$fingerprint" > "$stage/source/.proxyd-patch"

	# 只有完整补丁应用成功才替换生成目录；移动失败时恢复旧目录，避免留下半成品。
	if [ -e "$destination" ]; then
		mv "$destination" "$stage/previous"
	fi
	if ! mv "$stage/source" "$destination"; then
		if [ -d "$stage/previous" ]; then mv "$stage/previous" "$destination"; fi
		exit 1
	fi
	rm -rf "$stage"
	stage=""
}

prepare github.com/metacubex/mihomo patches/mihomo-concurrency.patch mihomo
# mihomo 的 tailscale 出站使用 metacubex/tailscale 分支；该分支与 tailcat 依赖的
# tailscale.com 共存于单二进制时，两份 tsweb/varz 的 init 重复注册同名 expvar 导致
# 进程启动即 panic（ADR 0002），补丁把重复注册降级为跳过。
prepare github.com/metacubex/tailscale patches/metacubex-tailscale-varz.patch metacubex-tailscale
