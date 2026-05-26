ARG GO_VERSION=1.26
FROM golang:${GO_VERSION} AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/
COPY hack/ hack/

ARG VERSION="dev"
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" \
    -o /out/controller ./cmd/controller

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/controller /controller
USER 65532:65532
ENTRYPOINT ["/controller"]
