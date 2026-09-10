VERSION ?= 0.1.0
GO ?= go
PLUGIN_ID = zai-coding-plan
OUT ?= dist/$(PLUGIN_ID)-v$(VERSION).so
ARCHIVE ?= dist/$(PLUGIN_ID)_$(VERSION)_linux_amd64.zip
CHECKSUMS ?= dist/checksums.txt
DEPLOY_DIR ?= ../../plugins/linux/amd64

.PHONY: fmt-check vet lint test test-release validate-source scan-secrets validate-release test-host-image build package deploy clean

fmt-check:
	test -z "$$(gofmt -l .)"

vet:
	$(GO) vet ./...

lint:
	golangci-lint run ./...

test:
	$(GO) test ./...

test-release:
	$(GO) test ./.github/scripts/release-validation
	.github/scripts/select-release-tag_test.sh

validate-source:
	$(GO) run -buildvcs=false ./.github/scripts/release-validation -mode source

scan-secrets:
	.github/scripts/gitleaks-scan.sh

validate-release:
	test -n "$(TAG)"
	$(GO) run -buildvcs=false ./.github/scripts/release-validation \
		-mode release -tag "$(TAG)" -version "$(VERSION)"

test-host-image:
	test -n "$(HOST_BINARY)"
	$(GO) run -buildvcs=false ./.github/scripts/host-integration \
		-host-binary "$(HOST_BINARY)" -plugin "$(OUT)"

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
		-archive "$(ARCHIVE)" -checksum "$(CHECKSUMS)"
	test -s "$(ARCHIVE)"
	test -s "$(CHECKSUMS)"

deploy: build
	mkdir -p "$(DEPLOY_DIR)"
	cp "$(OUT)" "$(DEPLOY_DIR)/"
	test -s "$(DEPLOY_DIR)/$(notdir $(OUT))"

clean:
	rm -rf dist
