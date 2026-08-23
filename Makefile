build:
	@go build -o bin/weavefs.exe ./cmd/weavefs

run: build
	@.\bin\weavefs.exe

# install puts the `weavefs` command on your PATH by building it into Go's bin
# directory (GOBIN, or GOPATH/bin — typically %USERPROFILE%\go\bin on Windows).
# After this, run `weavefs ...` from any directory instead of .\bin\weavefs.exe.
# If the command is not found afterwards, add that bin directory to your PATH.
install:
	@go install ./cmd/weavefs

test:
	@go test ./... -v
