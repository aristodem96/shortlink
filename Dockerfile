# ---- build stage -------------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies are cached in their own layer so code edits do not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/api ./cmd/api
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
    -o /out/shortlinkctl ./cmd/shortlinkctl
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/bot ./cmd/bot

# ---- runtime stage -----------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/api /api
COPY --from=build /out/shortlinkctl /shortlinkctl
COPY --from=build /out/bot /bot

USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/api"]
