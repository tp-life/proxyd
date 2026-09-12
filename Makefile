BINARY  := proxyd
PKG     := proxyd
CMD     := ./cmd/proxyd
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
# ADR 0002：单二进制默认打开 with_gvisor，解锁 mihomo tailscale 出站与 TUN gvisor/mixed 栈；
# 构建/测试/静态检查统一使用该标签，保证日常开发与发布产物覆盖同一份代码。
GOTAGS  := -tags with_gvisor
GORELEASER ?= goreleaser
TAG ?=

# TAG 只通过环境传给 shell，避免把用户输入直接拼进命令文本。
# make tag 的 recipe 会自行校验其 SemVer 格式、工作区状态和重复 tag。
export TAG

.PHONY: all deps build web test vet clean release tag

# all 先生成最新 Web 静态资源，再编译嵌入这些资源的 Go 单文件程序。
# 使用 `make` 或 `make all` 即可完成一次性完整构建，避免误用旧的 internal/api/dist。
all: web build

# build 仅编译 Go 程序，并嵌入 internal/api/dist 中当前已有的前端产物。
# 保留独立目标是为了让无 Node.js 的发布环境能够使用已经生成并提交的静态资源。
build: deps
	go build $(GOTAGS) -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)

# deps 下载固定版本并应用本地补丁（mihomo 并发修复、metacubex/tailscale varz 去重）；
# 完整源码仅保留在被忽略的 third_party。
# 返回：成功时可直接运行 go build/test；下载或补丁失败会终止依赖此目标的构建。
deps:
	sh scripts/prepare-mihomo.sh

# web 从 React 源码生成供 Go embed 打包的 internal/api/dist 静态资源。
web:
	npm --prefix web run build

test: deps
	go test $(GOTAGS) ./...

vet: deps
	go vet $(GOTAGS) ./...

clean:
	rm -rf bin dist

# release 使用与 GitHub Actions 相同的 GoReleaser 配置生成本地快照包。
# 参数：可用 GORELEASER=/path/to/goreleaser 覆盖命令位置；不接收发布 tag。
# 返回值：成功时在 dist/ 生成五个平台包、源码包和 SHA256SUMS，不创建 GitHub Release。
# 错误情况：缺少 GoReleaser、Web 构建失败或任一平台交叉编译失败时返回非零状态。
release: web deps
	@command -v "$(GORELEASER)" >/dev/null 2>&1 || { \
		echo "错误：未找到 goreleaser，请先安装 GoReleaser v2，或通过 GORELEASER 指定路径。" >&2; \
		exit 1; \
	}
	$(GORELEASER) release --snapshot --clean

# tag 创建并推送触发 GitHub Release 的 annotated tag。
# 参数：TAG，必须是 vMAJOR.MINOR.PATCH，可选 SemVer prerelease/build 后缀，例如 v1.2.3-rc.1。
# 返回值：校验与测试通过后，把当前 HEAD 的 annotated tag 推送到 origin。
# 错误情况：TAG 非法、工作区不干净、tag 已存在、Web 产物未提交、测试失败或 git push 失败时返回非零状态。
tag:
	@set -eu; \
		if ! printf '%s\n' "$$TAG" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$$'; then \
			echo "错误：请使用 make tag TAG=v1.2.3（也支持 v1.2.3-rc.1）。" >&2; \
			exit 1; \
		fi; \
		if [ -n "$$(git status --porcelain)" ]; then \
			echo "错误：工作区存在未提交变更，拒绝创建发布 tag。" >&2; \
			git status --short; \
			exit 1; \
		fi; \
		if git show-ref --verify --quiet "refs/tags/$$TAG"; then \
			echo "错误：tag $$TAG 已在本地存在。" >&2; \
			exit 1; \
		fi; \
		git remote get-url origin >/dev/null 2>&1 || { \
			echo "错误：未配置 origin，无法推送发布 tag。" >&2; \
			exit 1; \
		}; \
		$(MAKE) web test; \
		if [ -n "$$(git status --porcelain)" ]; then \
			echo "错误：构建后的 Web 产物与仓库不一致，请提交 internal/api/dist 后重试。" >&2; \
			git status --short; \
			exit 1; \
		fi; \
		git tag -a "$$TAG" -m "Release $$TAG"; \
		git push origin "$$TAG"
