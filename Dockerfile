# --- UI build stage: static JS/CSS is platform-independent ---
FROM --platform=$BUILDPLATFORM node:26-alpine AS ui-build
WORKDIR /ui
COPY ui/package.json ui/package-lock.json* ./
RUN if [ -f package-lock.json ]; then npm ci; else npm install; fi
COPY ui/ ./
RUN npm run build

# --- Build stage: cross-compile natively (no QEMU) ---
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH

WORKDIR /build

# Module graph only (cached unless go.mod/go.sum change)
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Source
COPY . .

# Drop in the freshly-built UI so //go:embed picks it up.
COPY --from=ui-build /ui/dist ./ui/dist

# Cross-compile for the target platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -installsuffix cgo -o /build/run cmd/main.go

# --- Final image ---
FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /build/config config
COPY --from=build /build/run .
CMD ["/app/run"]