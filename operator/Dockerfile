# Build the manager binary
FROM golang:1.25-alpine AS build
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux go build -o manager ./cmd

# Minimal runtime image. Static binary (CGO disabled) runs on alpine; the
# image runs as an unprivileged user to satisfy runAsNonRoot.
FROM alpine:3.20
WORKDIR /
COPY --from=build /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
