.PHONY: test

test:
	go test -race ./...
	@if [ -f plugin/go.mod ]; then cd plugin && go test ./...; fi
