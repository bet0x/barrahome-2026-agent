# Local development helpers. cgo links libsandlock_ffi from the in-tree
# sandlock checkout, so the FFI library must be built first.
SANDLOCK := third_party/sandlock
TAGS := -tags sandlock_repo

.PHONY: ffi build test run clean

ffi:
	cd $(SANDLOCK) && cargo build --release -p sandlock-ffi

build: ffi
	go build $(TAGS) -o barrahome-agent ./cmd/barrahome-agent

test: ffi
	go test $(TAGS) ./...

run: build
	./barrahome-agent serve

clean:
	rm -f barrahome-agent
