TAGS=with_gvisor,with_quic,with_dhcp,with_wireguard,with_utls,with_acme,with_clash_api,with_tailscale,with_conntrack

UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
   TAGS := standalone,$(TAGS)
endif

ifeq ($(OS),Windows_NT)
   LANTERND := lanternd.exe
   LANTERN := lantern.exe
else
   LANTERND := lanternd
   LANTERN := lantern
endif

VERSION ?=
LDFLAGS := $(if $(VERSION),-ldflags "-X 'github.com/getlantern/radiance/common.Version=$(VERSION)'")

.PHONY: build-daemon
build-daemon:
	go build -tags "$(TAGS)" $(LDFLAGS) -o bin/$(LANTERND) ./cmd/lanternd

.PHONY: run-daemon
run-daemon:
	go run -tags=$(TAGS) ./cmd/lanternd run \
		$(if $(data-path),--data-path=$(data-path)) \
		$(if $(log-path),--log-path=$(log-path)) \
		$(if $(log-level),--log-level=$(log-level))

.PHONY: build-cli
build-cli:
ifeq ($(UNAME_S),Darwin)
	go build -tags "standalone" $(LDFLAGS) -o bin/$(LANTERN) ./cmd/lantern
else
	go build $(LDFLAGS) -o bin/$(LANTERN) ./cmd/lantern
endif

.PHONY: build
build: build-daemon build-cli

# install* recipes compile binaries into $GOBIN. They do NOT register
# lanternd as a system service — use `lanternd install` for that.
.PHONY: install-daemon
install-daemon:
	go install -tags "$(TAGS)" $(LDFLAGS) ./cmd/lanternd

.PHONY: install-cli
install-cli:
ifeq ($(UNAME_S),Darwin)
	go install -tags "standalone" $(LDFLAGS) ./cmd/lantern
else
	go install $(LDFLAGS) ./cmd/lantern
endif

.PHONY: install
install: install-daemon install-cli

.PHONY: proto
proto:
	go build -o build/protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go
	protoc --go_out=. --plugin=build/protoc-gen-go --go_opt=paths=source_relative api/protos/auth.proto
	protoc --go_out=. --plugin=build/protoc-gen-go --go_opt=paths=source_relative api/protos/subscription.proto
	protoc --go_out=. --plugin=build/protoc-gen-go --go_opt=paths=source_relative issue/issue.proto

.PHONY: test
test:
	go test -v ./...
