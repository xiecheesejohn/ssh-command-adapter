.PHONY: build test check

build:
	go build -trimpath -o bin/ssh-command-adapter .

test:
	go test -race -timeout 90s ./...

check:
	go vet ./...
	@test -z "$$(gofmt -l *.go)"
