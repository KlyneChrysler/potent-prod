.PHONY: build run test clean fmt

BINARY := potent
PKG := ./cmd/potent

build:
	go build -o bin/$(BINARY) $(PKG)

run: build
	./bin/$(BINARY) -upstream http://localhost:9000

test:
	go test ./... -race -cover

fmt:
	go fmt ./...

clean:
	rm -rf bin/ potent.db
