VERSION ?= 0.0.1
GO ?= go
PLUGIN_ID = zai-coding-plan
OUT = dist/$(PLUGIN_ID)-v$(VERSION).so
ARCHIVE = dist/$(PLUGIN_ID)_$(VERSION)_linux_amd64.zip
ARCHIVE_CHECKSUM = $(ARCHIVE).sha256
DEPLOY_DIR ?= ../../plugins/linux/amd64

.PHONY: fmt-check vet lint test build package deploy clean

fmt-check:
	test -z "$$(gofmt -l .)"

vet:
	$(GO) vet ./...

lint:
	golangci-lint run ./...

test:
	$(GO) test ./...

build:
	test "$$($(GO) env GOOS)" = "linux"
	test "$$($(GO) env GOARCH)" = "amd64"
	mkdir -p "$(dir $(OUT))"
	CGO_ENABLED=1 $(GO) build -buildvcs=false -trimpath -buildmode=c-shared \
		-ldflags "-s -w -X main.pluginVersion=$(VERSION)" -o "$(OUT)" ./src
	rm -f dist/*.h
	test -s "$(OUT)"

package: build
	$(GO) run -buildvcs=false ./.github/scripts/package-release.go \
		-library "$(OUT)" -entry "$(PLUGIN_ID).so" \
		-archive "$(ARCHIVE)" -checksum "$(ARCHIVE_CHECKSUM)"
	cp "$(ARCHIVE_CHECKSUM)" dist/checksums.txt
	test -s dist/checksums.txt

deploy: build
	mkdir -p "$(DEPLOY_DIR)"
	cp "$(OUT)" "$(DEPLOY_DIR)/"
	test -s "$(DEPLOY_DIR)/$(notdir $(OUT))"

clean:
	rm -rf dist
