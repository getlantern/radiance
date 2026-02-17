TAGS=with_gvisor,with_quic,with_dhcp,with_wireguard,with_utls,with_acme,with_clash_api,with_tailscale

ifeq ($(OS),Windows_NT)
    EXECUTABLE := lanternd.exe
else
    EXECUTABLE := lanternd
endif

build-daemon:
	go build -tags "$(TAGS)" -o ../bin/$(EXECUTABLE) ./lanternd

run-daemon:
	go run -tags=$(TAGS) ./lanternd $(args)
