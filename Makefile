BINARY  := proxyd
PKG     := proxyd
CMD     := ./cmd/proxyd
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64

.PHONY: all build web test lint clean release

# all 先生成最新 Web 静态资源，再编译嵌入这些资源的 Go 单文件程序。
# 使用 `make` 或 `make all` 即可完成一次性完整构建，避免误用旧的 internal/api/dist。
all: web build

# build 仅编译 Go 程序，并嵌入 internal/api/dist 中当前已有的前端产物。
# 保留独立目标是为了让无 Node.js 的发布环境能够使用已经生成并提交的静态资源。
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)

# web 从 React 源码生成供 Go embed 打包的 internal/api/dist 静态资源。
web:
	npm --prefix web run build

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin dist

release: clean
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		out=dist/$(BINARY)_$${os}_$${arch}; \
		mkdir -p $$out; \
		bin=$(BINARY); [ "$$os" = windows ] && bin=$(BINARY).exe; \
		echo "==> $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $$out/$$bin $(CMD) || exit 1; \
		cp configs/config.example.yaml $$out/; \
		tar -czf $$out.tar.gz -C dist $(BINARY)_$${os}_$${arch}; \
	done
