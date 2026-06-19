build:
	@go build -o bin/fs.exe ./cmd/weavefs

run: build
	@.\bin\fs.exe

test:
	@go test ./... -v
