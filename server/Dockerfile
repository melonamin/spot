# Cross-compile on the build platform (fast, no QEMU) since the binary is
# pure Go (CGO_ENABLED=0). buildx populates BUILDPLATFORM/TARGETOS/TARGETARCH;
# plain `docker build` with BuildKit sets them to the host platform.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags "-s -w -X main.version=${VERSION}" -o /spot-api .

FROM alpine:3.22
COPY --from=build /spot-api /usr/local/bin/spot-api
RUN mkdir -p /data && chown nobody:nobody /data
USER nobody
ENTRYPOINT ["spot-api"]
