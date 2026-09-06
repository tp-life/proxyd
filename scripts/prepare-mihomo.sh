#!/bin/sh
# 准备带并发修复的 mihomo 本地依赖；无参数，无标准输出返回值，失败返回非零状态。
# 依赖 Go、Git、patch 和标准 Unix 工具；仅写项目 third_party，不修改 Go module cache。
# 完整源码不纳入版本控制，版本取自 go.mod，补丁和校验和随项目发布。
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
version=$(sed -n 's/^[[:space:]]*github.com\/metacubex\/mihomo \(v[^[:space:]]*\)$/\1/p' go.mod)
test -n "$version" || { echo "无法从 go.mod 读取 mihomo 版本" >&2; exit 1; }
patch_file="$root/patches/mihomo-concurrency.patch"
destination="$root/third_party/mihomo"
fingerprint=$({ printf '%s\n' "$version"; cat "$patch_file"; } | git hash-object --stdin)

# 相同版本和补丁重复执行不重新复制源码；升级版本或修改补丁后自动重新生成。
if [ -f "$destination/go.mod" ] && [ -f "$destination/.proxyd-patch" ] &&
   [ "$(cat "$destination/.proxyd-patch")" = "$fingerprint" ]; then
    exit 0
fi
mkdir -p "$root/third_party"
lock="$root/third_party/.prepare-lock"
# mkdir 原子抢占锁，避免多个 make 进程同时替换依赖；冲突时交由调用方重试。
mkdir "$lock" 2>/dev/null || { echo "另一个依赖准备进程正在运行，请稍后重试" >&2; exit 1; }
stage=""
trap 'test -z "$stage" || rm -rf "$stage"; rmdir "$lock"' EXIT
trap 'exit 1' HUP INT TERM

# Go 使用模块校验机制下载固定版本；先在临时目录应用补丁，失败时保留现有依赖。
go mod download "github.com/metacubex/mihomo@$version"
cache=$(go env GOMODCACHE)
stage=$(mktemp -d "$root/third_party/.mihomo.XXXXXX")
cp -R "$cache/github.com/metacubex/mihomo@$version" "$stage/source"
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
