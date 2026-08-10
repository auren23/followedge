BIN := bin/followedge
PKG := ./cmd/followedge

.PHONY: build test vet run clean

build:
	go build -o $(BIN) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

run:
	go run $(PKG) collect --config configs/observe.yaml

clean:
	rm -rf bin data
