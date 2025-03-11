proto:
	go build -o build/protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go
	protoc --go_out=. --plugin=build/protoc-gen-go --go_opt=paths=source_relative config/types.proto
	protoc --go_out=. --plugin=build/protoc-gen-go --go_opt=paths=source_relative user/auth.proto
