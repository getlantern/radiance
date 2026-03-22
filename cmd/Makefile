TAGS=with_gvisor,with_quic,with_dhcp,with_wireguard,with_utls,with_acme,with_clash_api,with_tailscale,with_conntrack

.PHONY: build-daemon
build-daemon:
	go build -tags "$(TAGS)" -o ../bin/lanternd ./lanternd

.PHONY: run-daemon
run-daemon:
	go run -tags=$(TAGS) ./lanternd/lanternd.go \
		$(if $(data-path),-data-path=$(data-path)) \
		$(if $(log-path),-log-path=$(log-path)) \
		$(if $(log-level),-log-level=$(log-level))

.PHONY: build-cli
build-cli:
	go build -o ../bin/lantern ./lantern
