.PHONY: build run test clean fmt eval eval-json

BINARY := potent
PKG := ./cmd/potent
EVAL_BINARY := potent-eval
EVAL_PKG := ./cmd/potent-eval

build:
	go build -o bin/$(BINARY) $(PKG)
	go build -o bin/$(EVAL_BINARY) $(EVAL_PKG)

run: build
	./bin/$(BINARY) -upstream http://localhost:9000

test:
	go test ./... -race -cover

fmt:
	go fmt ./...

# Run the offline semantic-dedup evaluation harness against the curated
# dataset under docs/eval. See docs/eval/methodology.md.
eval: build
	./bin/$(EVAL_BINARY) -dataset docs/eval/dataset.jsonl -policy docs/eval/policy.yaml

# Same eval but with machine-readable output for ci regression tracking.
eval-json: build
	./bin/$(EVAL_BINARY) -dataset docs/eval/dataset.jsonl -policy docs/eval/policy.yaml -json

clean:
	rm -rf bin/ potent.db
