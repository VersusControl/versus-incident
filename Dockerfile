FROM node:26-alpine AS ui-build
WORKDIR /ui
COPY ui/package.json ui/package-lock.json* ./
RUN if [ -f package-lock.json ]; then npm ci; else npm install; fi
COPY ui/ ./
RUN npm run build

FROM golang:1.26-alpine AS build

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
# Drop in the freshly-built UI so //go:embed picks it up.
COPY --from=ui-build /ui/dist ./ui/dist

# Builds the application as a staticly linked one, to allow it to run on alpine
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o run cmd/main.go

# Moving the binary to the 'final Image' to make it smaller
FROM alpine
WORKDIR /app
COPY --from=build /build/config config
COPY --from=build /build/run .
CMD ["/app/run"]