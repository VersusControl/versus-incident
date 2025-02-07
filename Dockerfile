FROM golang:1.23-alpine AS build

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .

# Builds the application as a staticly linked one, to allow it to run on alpine
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o run cmd/main.go

# Moving the binary to the 'final Image' to make it smaller
FROM alpine
WORKDIR /app
COPY --from=build /build/config config
COPY --from=build /build/run .
CMD ["/app/run"]