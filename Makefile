BINARIES := netcfgd netcfg-web
GOARCH   ?= amd64
LDFLAGS  := -s -w
DIST     := dist
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0)

.PHONY: all build linux amd64 arm64 armv7 deb deb-amd64 deb-arm64 deb-armv7 vet test fuzz cover clean

all: vet test build

build:
	@for b in $(BINARIES); do go build -ldflags "$(LDFLAGS)" -o $(DIST)/$$b ./cmd/$$b; done

# Cross-compile for the target device. CGO is off so the binaries are static.
linux:
	@for b in $(BINARIES); do \
		GOOS=linux GOARCH=$(GOARCH) CGO_ENABLED=0 \
		go build -ldflags "$(LDFLAGS)" -o $(DIST)/$$b ./cmd/$$b; \
	done
	@echo "Built $(BINARIES) for linux/$(GOARCH) in $(DIST)/"

amd64:
	$(MAKE) linux GOARCH=amd64

arm64:
	$(MAKE) linux GOARCH=arm64

armv7:
	@for b in $(BINARIES); do \
		GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 \
		go build -ldflags "$(LDFLAGS)" -o $(DIST)/$$b ./cmd/$$b; \
	done

# Packaging needs dpkg-deb, so run these on a Debian host, in WSL or in CI.
deb: linux
	VERSION=$(VERSION) sh deploy/build-deb.sh $(GOARCH)

deb-amd64:
	$(MAKE) deb GOARCH=amd64

deb-arm64:
	$(MAKE) deb GOARCH=arm64

deb-armv7: armv7
	VERSION=$(VERSION) sh deploy/build-deb.sh arm

vet:
	go vet ./...
	gofmt -l .

test:
	go test ./...

fuzz:
	go test -run=NONE -fuzz=FuzzParseScanResults -fuzztime=60s ./internal/adapters/wpactrl

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

clean:
	rm -rf $(DIST) coverage.out
