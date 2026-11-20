# One image with the gateway and the demo's helper, built together so the modules are
# downloaded and compiled once.
FROM golang:1.26@sha256:cfa27959837144bf4649802ba38e59daffd0dd2a7994aa83daaf113285b0f551 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=demo" -o /out/ ./cmd/transponder ./demo/sim \
    && mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/transponder /out/sim /
# The SQLite store lives here, in a volume the seed step and the gateway share.
COPY --from=build --chown=65532:65532 /out/data /data
WORKDIR /data
