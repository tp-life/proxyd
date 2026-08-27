BINARY  := proxyd
PKG     := proxyd
CMD     := ./cmd/proxyd
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64

.PHONY: build test lint clean release

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)

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
