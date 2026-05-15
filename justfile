base_tags := "with_gvisor,with_quic,with_wireguard,with_utls,with_grpc,with_conntrack"
tags := if os() == "macos" { "standalone," + base_tags } else { base_tags }
lanternd := if os() == "windows" { "lanternd.exe" } else { "lanternd" }
lantern := if os() == "windows" { "lantern.exe" } else { "lantern" }
version := env("VERSION", "")
ldflags := if version != "" { "-ldflags \"-X 'github.com/getlantern/radiance/common.Version=" + version + "'\"" } else { "" }

cli_tags := if os() == "macos" { "standalone" } else { "" }
cli_tags_flag := if cli_tags != "" { "-tags " + cli_tags } else { "" }

build-daemon:
    go build -tags "{{tags}}" {{ldflags}} -o bin/{{lanternd}} ./cmd/lanternd

run-daemon *args:
    go run -tags={{tags}} ./cmd/lanternd run {{args}}

build-cli:
    go build {{cli_tags_flag}} {{ldflags}} -o bin/{{lantern}} ./cmd/lantern

build: build-daemon build-cli

# install* recipes compile binaries into $GOBIN. They do NOT register
# lanternd as a system service — use `lanternd install` for that.
install-daemon:
    go install -tags "{{tags}}" {{ldflags}} ./cmd/lanternd

install-cli:
    go install {{cli_tags_flag}} {{ldflags}} ./cmd/lantern

install: install-daemon install-cli

proto:
    go build -o build/protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go
    protoc --go_out=. --plugin=build/protoc-gen-go --go_opt=paths=source_relative api/protos/auth.proto
    protoc --go_out=. --plugin=build/protoc-gen-go --go_opt=paths=source_relative api/protos/subscription.proto
    protoc --go_out=. --plugin=build/protoc-gen-go --go_opt=paths=source_relative issue/issue.proto

test:
    go test -v ./...
