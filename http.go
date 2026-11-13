package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// The site is compiled into the binary, so a single artifact can be shipped
// to AWS without any extra files next to it.
//
//go:embed sites/root
var site embed.FS

func helloHandler(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")

	if name == "" {
		name = "World"
	}

	fmt.Fprintf(w, "Hello, %s!", name)
}

// healthHandler is what a load balancer (ALB, App Runner health check) polls.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
}

func newHandler() (http.Handler, error) {
	root, err := fs.Sub(site, "sites/root")
	if err != nil {
		return nil, fmt.Errorf("sub sites/root: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/hello", helloHandler)
	mux.HandleFunc("/healthz", healthHandler)
	mux.Handle("/", http.FileServer(http.FS(root)))

	return mux, nil
}

// port returns the port AWS tells us to listen on, falling back to 8080 locally.
func port() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}

	return "8080"
}

func main() {
	handler, err := newHandler()
	if err != nil {
		log.Fatal(err)
	}

	addr := ":" + port()
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// AWS sends SIGTERM on deploy/scale-in; use it to drain in-flight requests.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("Server running at http://localhost%s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
