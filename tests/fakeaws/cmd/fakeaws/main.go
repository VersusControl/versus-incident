package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "listen address")
	scenario := flag.String("scenario", fakeaws.DefaultScenario, "fixture scenario")
	flag.Parse()

	server, err := fakeaws.NewServer(*scenario)
	if err != nil {
		_, _ = os.Stderr.WriteString("fakeaws: " + err.Error() + "\n")
		os.Exit(2)
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		_, _ = os.Stderr.WriteString("fakeaws: listen failed\n")
		os.Exit(1)
	}
	httpServer := &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	_, _ = os.Stdout.WriteString("fakeaws listening on http://" + listener.Addr().String() +
		" scenario=" + *scenario + "\n")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			os.Exit(1)
		}
	}
}
