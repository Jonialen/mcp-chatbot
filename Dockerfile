# Build stage.
#
# The binary is linked statically so the final image needs no libc: CGO_ENABLED=0
# is what makes a Go binary self-contained, and without it the build succeeds and
# the container fails to start with an error about a missing dynamic loader.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies are copied first so a change to the source does not invalidate the
# layer that downloaded them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# -trimpath keeps build machine paths out of the binary.
# -ldflags="-s -w" drops the symbol table and DWARF data, which this server has
# no use for and which are most of its size.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /out/netprobe \
        ./cmd/netprobe

# Runtime stage.
#
# distroless/static carries root certificates and time zone data and nothing
# else: no shell, no package manager, no utilities. netprobe opens TLS
# connections, so an image without a certificate bundle would fail every https
# probe with an unverifiable-authority error that looks like a network fault.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/netprobe /netprobe

# Documents the default. The port is read from $PORT at runtime, because a cloud
# runtime assigns one and routes to whatever the container binds.
EXPOSE 8080

# nonroot, from the base image: a process that probes the network on behalf of
# its callers has no reason to be able to write to its own filesystem.
USER nonroot:nonroot

ENTRYPOINT ["/netprobe"]
