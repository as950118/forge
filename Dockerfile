# forge serve-api: read-only HTTP API over the exported snapshot (TASK-0037).
# The image holds no secrets: FORGE_EXPORT_URL, FORGE_API_TOKEN and FORGE_PG_CA
# are supplied by the platform at run time. $PORT is read by the program itself.
ARG GO_VERSION=1.27

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY db ./db
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/forge ./cmd/forge

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/forge /usr/local/bin/forge
USER 65532:65532
# Default for hosts without $PORT; Render overrides it with its own value.
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/forge", "serve-api"]
