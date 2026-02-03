TAGS=with_gvisor,with_quic,with_dhcp,with_wireguard,with_utls,with_acme,with_clash_api,with_tailscale

build-daemon:
	go build -tags "$(TAGS)" -o ../bin/lanternd ./lanternd/lanternd.go

run-daemon:
	go run -tags=$(TAGS) ./lanternd/lanternd.go $(args)
