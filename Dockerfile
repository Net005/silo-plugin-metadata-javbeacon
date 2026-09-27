# Packages the plugin binary (+ its embedded manifest.json) as an OCI image
# for GHCR distribution/archival. Silo itself installs this plugin from a
# binary upload or a repo build per its own plugin-install flow, not by
# pulling this image directly - see README.md - but publishing it to GHCR
# alongside the raw per-platform binaries (release.yml) gives every consumer
# a pinned, reproducible artifact to pull from either place.
FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/plugin .

FROM gcr.io/distroless/static-debian12:nonroot
# manifest.json is embedded into the binary at build time (see main.go's
# //go:embed) and its checksum is computed from this exact executable at
# startup, so nothing else needs to be copied into the final image.
COPY --from=build /out/plugin /plugin
USER nonroot:nonroot
ENTRYPOINT ["/plugin"]
