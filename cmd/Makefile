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
	go build -tags "$(TAGS)" $(LDFLAGS) -o ../bin/$(LANTERND) ./lanternd

.PHONY: run-daemon
run-daemon:
	go run -tags=$(TAGS) ./lanternd run \
		$(if $(data-path),--data-path=$(data-path)) \
		$(if $(log-path),--log-path=$(log-path)) \
		$(if $(log-level),--log-level=$(log-level))

.PHONY: build-cli
build-cli:
ifeq ($(UNAME_S),Darwin)
	go build -tags "standalone" $(LDFLAGS) -o ../bin/$(LANTERN) ./lantern
else
	go build $(LDFLAGS) -o ../bin/$(LANTERN) ./lantern
endif

.PHONY: build
build: build-daemon build-cli
