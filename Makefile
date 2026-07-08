.PHONY: lint test build

lint:
	gofmt -l .
	go vet ./...

test:
	go test ./...

build:
	go build -o dockmind ./cmd/dockmind
