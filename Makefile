proto:
	go build -o build/protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go
	go build -o build/protoc-gen-go-grpc google.golang.org/grpc/cmd/protoc-gen-go-grpc

	protoc --go_out=. --go-grpc_out=. \
		--plugin=protoc-gen-go=build/protoc-gen-go \
		--plugin=protoc-gen-go-grpc=build/protoc-gen-go-grpc \
		--go_opt=paths=source_relative \
		--go-grpc_opt=paths=source_relative \
		config/types.proto

	protoc --go_out=. --go-grpc_out=. \
		--plugin=protoc-gen-go=build/protoc-gen-go \
		--plugin=protoc-gen-go-grpc=build/protoc-gen-go-grpc \
		--go_opt=paths=source_relative \
		--go-grpc_opt=paths=source_relative \
		api/protos/auth.proto

	protoc --go_out=. --go-grpc_out=. \
		--plugin=protoc-gen-go=build/protoc-gen-go \
		--plugin=protoc-gen-go-grpc=build/protoc-gen-go-grpc \
		--go_opt=paths=source_relative \
		--go-grpc_opt=paths=source_relative \
		api/protos/vpn.proto

	protoc --go_out=. --go-grpc_out=. \
		--plugin=protoc-gen-go=build/protoc-gen-go \
		--plugin=protoc-gen-go-grpc=build/protoc-gen-go-grpc \
		--go_opt=paths=source_relative \
		--go-grpc_opt=paths=source_relative \
		issue/issue.proto

mock:
	go install go.uber.org/mock/mockgen@latest
	go generate ./...

test:
	go test -v ./...
