BINARY  := o365-log-extractor
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
IMAGE   := ghcr.io/alex-j-butler/$(BINARY)

# Platforms cross-compiled by `make dist`; mirrors .github/workflows/release.yml.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build test vet fmt docker dist clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	go fmt ./...

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(BINARY):latest .

# Reproduce the release artifacts locally.
dist:
	@mkdir -p dist
	@for target in $(PLATFORMS); do \
		os=$${target%/*}; arch=$${target#*/}; \
		name=$(BINARY); \
		if [ "$$os" = windows ]; then name=$$name.exe; fi; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags "$(LDFLAGS)" \
				-o "dist/$(BINARY)_$(VERSION)_$${os}_$${arch}/$$name" ./cmd/$(BINARY) || exit 1; \
	done

clean:
	rm -rf bin dist
