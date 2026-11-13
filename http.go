package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"
)

// Every directory under sites/ is compiled into the binary and served as its
// own hostname, so a single artifact can be shipped to AWS with no side files.
//
//go:embed sites
var sitesFS embed.FS

const defaultBaseDomain = "solustrum.com"

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
	sites, err := loadSites()
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/hello", helloHandler)
	mux.HandleFunc("/healthz", healthHandler)

	// The Cloudflare tunnel points straight at this process, so the deploy
	// webhook has to be forwarded to its own listener. Unset = disabled.
	if target := os.Getenv("WEBHOOK_URL"); target != "" {
		u, err := url.Parse(target)
		if err != nil {
			return nil, fmt.Errorf("parse WEBHOOK_URL: %w", err)
		}
		mux.Handle("/deploy/solustrum", httputil.NewSingleHostReverseProxy(u))
	}

	mux.Handle("/", siteRouter(sites))

	return mux, nil
}

// loadSites turns every directory in sites/ into a handler. Adding
// sites/<name> serves <name>.<base> automatically; sites/root is the apex.
func loadSites() (map[string]http.Handler, error) {
	entries, err := fs.ReadDir(sitesFS, "sites")
	if err != nil {
		return nil, fmt.Errorf("read sites: %w", err)
	}

	handlers := make(map[string]http.Handler, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		root, err := fs.Sub(sitesFS, "sites/"+entry.Name())
		if err != nil {
			return nil, fmt.Errorf("sub sites/%s: %w", entry.Name(), err)
		}

		handlers[entry.Name()] = staticHandler(root)
	}

	if _, ok := handlers["root"]; !ok {
		return nil, errors.New("sites/root is missing")
	}

	return handlers, nil
}

// siteRouter picks the site from the Host header: the apex and localhost serve
// sites/root, <name>.<base> serves sites/<name>, anything else falls back to
// the root site so bare IP access still works.
func siteRouter(sites map[string]http.Handler) http.Handler {
	base := os.Getenv("BASE_DOMAIN")
	if base == "" {
		base = defaultBaseDomain
	}
	base = strings.ToLower(strings.TrimSuffix(base, "."))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler, ok := siteFor(r.Host, base, sites)
		if !ok {
			http.NotFound(w, r)
			return
		}

		handler.ServeHTTP(w, r)
	})
}

func siteFor(host, base string, sites map[string]http.Handler) (http.Handler, bool) {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")

	switch host {
	case base, "www." + base, "localhost", "127.0.0.1", "::1":
		handler, ok := sites["root"]

		return handler, ok
	}

	// <name>.<base> and <name>.localhost (handy locally) map to sites/<name>.
	for _, suffix := range []string{"." + base, ".localhost"} {
		if name, found := strings.CutSuffix(host, suffix); found {
			handler, ok := sites[name]

			return handler, ok
		}
	}

	handler, ok := sites["root"]

	return handler, ok
}

// staticHandler serves the embedded site. Files carry a content-hash ETag and
// "no-cache", so browsers and Cloudflare revalidate (cheap 304) instead of
// replaying stale bytes after a deploy.
func staticHandler(root fs.FS) http.Handler {
	etags := make(map[string]string)
	_ = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(root, p)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(data)
		etags["/"+p] = `"` + hex.EncodeToString(sum[:8]) + `"`
		return nil
	})

	files := http.FileServer(http.FS(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tag, ok := etags[assetName(r.URL.Path)]; ok {
			w.Header().Set("ETag", tag)
			w.Header().Set("Cache-Control", "no-cache")
		}

		files.ServeHTTP(w, r)
	})
}

// assetName maps a request path to the embedded file that answers it.
func assetName(p string) string {
	name := strings.TrimPrefix(path.Clean("/"+p), "/")
	if name == "" || strings.HasSuffix(p, "/") {
		name = path.Join(name, "index.html")
	}

	return "/" + name
}

// port returns the port AWS tells us to listen on, falling back to 8080 locally.
func port() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}

	return "8080"
}

// listenAddr binds every interface by default (containers need that). Set HOST
// to 127.0.0.1 behind a reverse proxy so only the proxy can reach us.
func listenAddr() string {
	return net.JoinHostPort(os.Getenv("HOST"), port())
}

func main() {
	handler, err := newHandler()
	if err != nil {
		log.Fatal(err)
	}

	addr := listenAddr()
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// AWS sends SIGTERM on deploy/scale-in; use it to drain in-flight requests.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("Server listening on %s", addr)
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
