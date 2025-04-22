proto:
	go build -o build/protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go
	protoc --go_out=. --plugin=build/protoc-gen-go --go_opt=paths=source_relative config/types.proto
	protoc --go_out=. --plugin=build/protoc-gen-go --go_opt=paths=source_relative user/protos/auth.proto
	protoc --go_out=. --plugin=build/protoc-gen-go --go_opt=paths=source_relative user/protos/vpn.proto
	protoc --go_out=. --plugin=build/protoc-gen-go --go_opt=paths=source_relative issue/issue.proto

mock:
	go install go.uber.org/mock/mockgen@latest
	go generate ./...

test:
	go test -v ./...
