# syntax=docker/dockerfile:1

# The Go SDK links sandlock's C ABI through cgo, so CGO_ENABLED=0 is not an
# option and the native library has to be built first.
#
# glibc, not musl. sandlock-core did not compile against musl at all until
# v0.8.8 (the libc crate types the ptrace request as c_uint on glibc and c_int
# on musl, among others), and now it does, so a small Alpine image is finally
# possible. This build stays on glibc anyway: upstream publishes no musl target
# in its release matrix, the FFI cdylib needs -C target-feature=-crt-static to
# build for musl, and debian:trixie-slim matches the VPS's glibc 2.41, so the
# binary tested locally is the binary that runs in production.
FROM rust:1-bookworm AS ffi
WORKDIR /src
COPY third_party/sandlock/ ./third_party/sandlock/
WORKDIR /src/third_party/sandlock
RUN cargo build --release -p sandlock-ffi

FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY third_party/sandlock/ ./third_party/sandlock/
COPY --from=ffi /src/third_party/sandlock/target/release/libsandlock_ffi.so ./third_party/sandlock/target/release/
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=1 go build -tags sandlock_repo -o /barrahome-agent ./cmd/barrahome-agent

FROM debian:trixie-slim
# wget backs compose's healthcheck; nothing else here needs a shell tool.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates wget && rm -rf /var/lib/apt/lists/*
COPY --from=build /barrahome-agent /usr/local/bin/barrahome-agent
COPY --from=ffi /src/third_party/sandlock/target/release/libsandlock_ffi.so /usr/local/lib/
ENV LD_LIBRARY_PATH=/usr/local/lib
# HOME points at the mounted content so nothing resembling a real user home
# exists in this image: no .ssh, no .env, no shell history, no git config.
ENV HOME=/workspace
WORKDIR /workspace
EXPOSE 9000
# The agent is PID 1 and confines its own worker.
ENTRYPOINT ["/usr/local/bin/barrahome-agent", "supervise"]
