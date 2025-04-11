module github.com/getlantern/radiance

go 1.23.6

toolchain go1.24.1

replace github.com/sagernet/sing-box => github.com/getlantern/sing-box-minimal v1.11.6-0.20250319162213-b56a0b17a972

replace github.com/sagernet/wireguard-go => github.com/getlantern/wireguard-go v0.0.1-beta.5.0.20250310145906-45220d8aec77

//replace github.com/sagernet/sing-box => ../sing-box-minimal
//replace github.com/getlantern/common => ../common

require (
	github.com/1Password/srp v0.2.0
	github.com/Jigsaw-Code/outline-sdk v0.0.19
	github.com/Jigsaw-Code/outline-sdk/x v0.0.2
	github.com/getlantern/algeneva v0.0.0-20250307163401-1824e7b54f52
	github.com/getlantern/appdir v0.0.0-20250324200952-507a0625eb01
	github.com/getlantern/common v1.2.1-0.20250404213255-37d58e3e0fae
	github.com/getlantern/fronted v0.0.0-20250330001402-75899df1c2cd
	github.com/getlantern/golog v0.0.0-20230503153817-8e72de7e0a65
	github.com/getlantern/jibber_jabber v0.0.0-20210901195950-68955124cc42
	github.com/getlantern/kindling v0.0.0-20250401200447-f536cbd057e6
	github.com/getlantern/sing-box-extensions v0.0.0-20250402200957-bb61b4768333
	github.com/getlantern/timezone v0.0.0-20210901200113-3f9de9d360c9
	github.com/gobwas/ws v1.4.0
	github.com/google/uuid v1.6.0
	github.com/sagernet/sing v0.6.4
	github.com/sagernet/sing-box v1.11.5
	github.com/stretchr/testify v1.10.0
	go.opentelemetry.io/otel/log v0.11.0
	go.opentelemetry.io/otel/sdk v1.35.0
	go.opentelemetry.io/otel/sdk/metric v1.35.0
	go.uber.org/mock v0.5.0
	google.golang.org/protobuf v1.36.5
)

require (
	github.com/alitto/pond/v2 v2.1.5 // indirect
	github.com/blang/semver v3.5.1+incompatible // indirect
	github.com/caddyserver/zerossl v0.1.3 // indirect
	github.com/dsnet/compress v0.0.2-0.20210315054119-f66993602bf5 // indirect
	github.com/getlantern/errors v1.0.4 // indirect
	github.com/getlantern/keepcurrent v0.0.0-20240126172110-2e0264ca385d // indirect
	github.com/getlantern/tlsdialer/v3 v3.0.3 // indirect
	github.com/goccy/go-yaml v1.15.13 // indirect
	github.com/golang/snappy v0.0.4 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/klauspost/pgzip v1.2.5 // indirect
	github.com/mholt/acmez v1.2.0 // indirect
	github.com/mholt/acmez/v3 v3.0.1 // indirect
	github.com/mholt/archiver/v3 v3.5.1 // indirect
	github.com/nwaples/rardecode v1.1.0 // indirect
	github.com/onsi/ginkgo v1.16.5 // indirect
	github.com/onsi/gomega v1.36.2 // indirect
	github.com/oschwald/maxminddb-golang v1.12.0 // indirect
	github.com/pivotal-cf-experimental/jibber_jabber v0.0.0-20151120183258-bcc4c8345a21 // indirect
	github.com/refraction-networking/utls v1.6.7 // indirect
	github.com/sagernet/cloudflare-tls v0.0.0-20231208171750-a4483c1b7cd1 // indirect
	github.com/tevino/abool/v2 v2.1.0 // indirect
	github.com/tkuchiki/go-timezone v0.2.0 // indirect
	github.com/ulikunitz/xz v0.5.10 // indirect
	github.com/xi2/xz v0.0.0-20171230120015-48954b6210f8 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	go.uber.org/zap/exp v0.3.0 // indirect
	golang.org/x/text v0.22.0 // indirect
)

require (
	dario.cat/mergo v1.0.1
	github.com/ajg/form v1.5.1 // indirect
	github.com/andybalholm/brotli v1.1.0 // indirect
	github.com/caddyserver/certmagic v0.21.7 // indirect
	github.com/cloudflare/circl v1.6.0 // indirect
	github.com/cretz/bine v0.2.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/fsnotify/fsnotify v1.7.0 // indirect
	github.com/getlantern/byteexec v0.0.0-20220903142956-e6ed20032cfd // indirect
	github.com/getlantern/context v0.0.0-20220418194847-3d5e7a086201 // indirect
	github.com/getlantern/elevate v0.0.0-20220903142053-479ab992b264 // indirect
	github.com/getlantern/fdcount v0.0.0-20210503151800-5decd65b3731 // indirect
	github.com/getlantern/filepersist v0.0.0-20210901195658-ed29a1cb0b7c // indirect
	github.com/getlantern/hex v0.0.0-20220104173244-ad7e4b9194dc // indirect
	github.com/getlantern/hidden v0.0.0-20220104173330-f221c5a24770 // indirect
	github.com/getlantern/iptool v0.0.0-20230112135223-c00e863b2696 // indirect
	github.com/getlantern/keyman v0.0.0-20230503155501-4e864ca2175b // indirect
	github.com/getlantern/mtime v0.0.0-20200417132445-23682092d1f7 // indirect
	github.com/getlantern/netx v0.0.0-20240830183145-c257516187f0 // indirect
	github.com/getlantern/ops v0.0.0-20231025133620-f368ab734534 // indirect
	github.com/getlantern/osversion v0.0.0-20240418205916-2e84a4a4e175
	github.com/getsentry/sentry-go v0.31.1
	github.com/go-chi/chi/v5 v5.2.1 // indirect
	github.com/go-chi/render v1.0.3 // indirect
	github.com/go-logr/logr v1.4.2 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/go-stack/stack v1.8.1 // indirect
	github.com/gobwas/httphead v0.1.0 // indirect
	github.com/gobwas/pool v0.2.1 // indirect
	github.com/gofrs/uuid/v5 v5.3.1 // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/hashicorp/yamux v0.1.2 // indirect
	github.com/insomniacslk/dhcp v0.0.0-20250109001534-8abf58130905 // indirect
	github.com/josharian/native v1.1.1-0.20230202152459-5c7d0dd6ab86 // indirect
	github.com/klauspost/compress v1.17.11 // indirect
	github.com/klauspost/cpuid/v2 v2.2.9 // indirect
	github.com/libdns/alidns v1.0.3 // indirect
	github.com/libdns/cloudflare v0.1.1 // indirect
	github.com/libdns/libdns v0.2.2 // indirect
	github.com/logrusorgru/aurora v2.0.3+incompatible // indirect
	github.com/mdlayher/netlink v1.7.2 // indirect
	github.com/mdlayher/socket v0.5.1 // indirect
	github.com/metacubex/tfo-go v0.0.0-20241231083714-66613d49c422 // indirect
	github.com/miekg/dns v1.1.63 // indirect
	github.com/oxtoacart/bpool v0.0.0-20190530202638-03653db5a59c // indirect
	github.com/pierrec/lz4/v4 v4.1.21 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/quic-go/qpack v0.5.1 // indirect
	github.com/quic-go/qtls-go1-20 v0.4.1 // indirect
	github.com/sagernet/bbolt v0.0.0-20231014093535-ea5cb2fe9f0a // indirect
	github.com/sagernet/cors v1.2.1 // indirect
	github.com/sagernet/fswatch v0.1.1 // indirect
	github.com/sagernet/gvisor v0.0.0-20241123041152-536d05261cff
	github.com/sagernet/netlink v0.0.0-20240612041022-b9a21c07ac6a // indirect
	github.com/sagernet/nftables v0.3.0-beta.4 // indirect
	github.com/sagernet/quic-go v0.49.0-beta.1 // indirect
	github.com/sagernet/reality v0.0.0-20230406110435-ee17307e7691 // indirect
	github.com/sagernet/sing-dns v0.4.0
	github.com/sagernet/sing-mux v0.3.1 // indirect
	github.com/sagernet/sing-quic v0.4.1-beta.1 // indirect
	github.com/sagernet/sing-shadowsocks v0.2.7 // indirect
	github.com/sagernet/sing-shadowsocks2 v0.2.0 // indirect
	github.com/sagernet/sing-shadowtls v0.2.0 // indirect
	github.com/sagernet/sing-tun v0.6.1
	github.com/sagernet/sing-vmess v0.2.0 // indirect
	github.com/sagernet/smux v0.0.0-20231208180855-7041f6ea79e7 // indirect
	github.com/sagernet/utls v1.6.7 // indirect
	github.com/sagernet/wireguard-go v0.0.1-beta.5
	github.com/sagernet/ws v0.0.0-20231204124109-acfe8907c854 // indirect
	github.com/shadowsocks/go-shadowsocks2 v0.1.5 // indirect
	github.com/stretchr/objx v0.5.2 // indirect
	github.com/u-root/uio v0.0.0-20240118234441-a3c409a6018e // indirect
	github.com/vishvananda/netns v0.0.4 // indirect
	github.com/zeebo/blake3 v0.2.4 // indirect
	go.opentelemetry.io/otel v1.35.0
	go.opentelemetry.io/otel/exporters/stdout/stdoutlog v0.11.0
	go.opentelemetry.io/otel/exporters/stdout/stdoutmetric v1.35.0
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.35.0
	go.opentelemetry.io/otel/metric v1.35.0
	go.opentelemetry.io/otel/sdk/log v0.11.0
	go.opentelemetry.io/otel/trace v1.35.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.27.0 // indirect
	go4.org/netipx v0.0.0-20231129151722-fdeea329fbba
	golang.org/x/crypto v0.33.0
	golang.org/x/exp v0.0.0-20240719175910-8a7402abbf56 // indirect
	golang.org/x/mod v0.23.0 // indirect
	golang.org/x/net v0.35.0 // indirect
	golang.org/x/sync v0.11.0 // indirect
	golang.org/x/sys v0.30.0
	golang.org/x/time v0.7.0 // indirect
	golang.org/x/tools v0.28.0 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20241202173237-19429a94021a // indirect
	google.golang.org/grpc v1.70.0 // indirect
	gopkg.in/yaml.v3 v3.0.1
	lukechampine.com/blake3 v1.3.0 // indirect
)
