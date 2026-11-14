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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Every directory under sites/ is compiled into the binary and served as its
// own hostname, so a single artifact can be shipped to AWS with no side files.
//
// all: is required so dot-directories are embedded too; sites/.err_404 holds
// custom error pages. Serving code still refuses dot paths (see staticHandler).
//
//go:embed all:sites
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
	errorPages := loadErrorPages()
	notFound := func(w http.ResponseWriter, r *http.Request) {
		if page, ok := errorPages[http.StatusNotFound]; ok {
			page.serve(w, r, http.StatusNotFound)
			return
		}

		http.NotFound(w, r)
	}

	sites, err := loadSites(notFound)
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

	mux.Handle("/", siteRouter(sites, notFound))

	return mux, nil
}

// loadSites decides what lives in sites/: hidden or underscore-prefixed names
// are skipped (sites/.err_404 is for error pages, not a site). A folder with
// html is a site and answers its own index; a folder without html is served as
// a plain file tree, so a CDN folder needs no index.html and gets no directory
// listing. sites/root answers the apex; every other name gets <name>.<base> and
// a path prefix on the apex.
func loadSites(notFound http.HandlerFunc) (map[string]http.Handler, error) {
	entries, err := fs.ReadDir(sitesFS, "sites")
	if err != nil {
		return nil, fmt.Errorf("read sites: %w", err)
	}

	handlers := make(map[string]http.Handler, len(entries))
	var accepted, skipped []string

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			skipped = append(skipped, name+" (hidden)")
			continue
		}

		root, err := fs.Sub(sitesFS, "sites/"+name)
		if err != nil {
			return nil, fmt.Errorf("sub sites/%s: %w", name, err)
		}

		withIndex := hasHTML(root)
		if withIndex {
			accepted = append(accepted, name+" (site)")
		} else {
			accepted = append(accepted, name+" (files)")
		}

		handlers[name] = staticHandler(root, withIndex, notFound)
	}

	if _, ok := handlers["root"]; !ok {
		return nil, errors.New("sites/root is missing")
	}

	sort.Strings(accepted)
	sort.Strings(skipped)
	log.Printf("sites: %s", strings.Join(accepted, ", "))
	if len(skipped) > 0 {
		log.Printf("sites skipped: %s", strings.Join(skipped, ", "))
	}

	return handlers, nil
}

// hasHTML reports whether a site folder contains at least one page.
func hasHTML(root fs.FS) bool {
	found := false

	_ = fs.WalkDir(root, ".", func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}

		if strings.EqualFold(path.Ext(p), ".html") {
			found = true

			return fs.SkipAll
		}

		return nil
	})

	return found
}

// siteRouter serves every folder in sites/ two ways: as its own hostname and
// as a path prefix on the apex, so sites/website answers both
// website.<base> and <base>/website/. sites/root is the apex itself, and
// anything nested stays a path under both.
func siteRouter(sites map[string]http.Handler, notFound http.HandlerFunc) http.Handler {
	base := os.Getenv("BASE_DOMAIN")
	if base == "" {
		base = defaultBaseDomain
	}
	base = strings.ToLower(strings.TrimSuffix(base, "."))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, handler, ok := siteFor(r.Host, base, sites)
		if !ok {
			notFound(w, r)
			return
		}

		if name == "root" {
			if sub, p, found := pathSite(sites, r.URL.Path); found {
				r = r.Clone(r.Context())
				r.URL.Path = p
				r.URL.RawPath = ""
				handler = sub
			}
		}

		handler.ServeHTTP(w, r)
	})
}

// pathSite maps /<name>/rest onto sites/<name> for hosts that serve the root
// site, so <base>/<name>/rest mirrors <name>.<base>/rest.
func pathSite(sites map[string]http.Handler, p string) (http.Handler, string, bool) {
	name, rest, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	if name == "" || name == "root" {
		return nil, "", false
	}

	handler, ok := sites[name]
	if !ok {
		return nil, "", false
	}

	return handler, "/" + rest, true
}

func siteFor(host, base string, sites map[string]http.Handler) (string, http.Handler, bool) {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")

	switch host {
	case base, "www." + base, "localhost", "127.0.0.1", "::1":
		handler, ok := sites["root"]

		return "root", handler, ok
	}

	// <name>.<base> and <name>.localhost (handy locally) map to sites/<name>.
	for _, suffix := range []string{"." + base, ".localhost"} {
		if name, found := strings.CutSuffix(host, suffix); found {
			handler, ok := sites[name]

			return name, handler, ok
		}
	}

	handler, ok := sites["root"]

	return "root", handler, ok
}

// staticHandler serves the embedded site. Files carry a content-hash ETag and
// "no-cache", so browsers and Cloudflare revalidate (cheap 304) instead of
// replaying stale bytes after a deploy.
func staticHandler(root fs.FS, withIndex bool, notFound http.HandlerFunc) http.Handler {
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
		// Hidden files are embedded for error pages, never served.
		if hasDotSegment(r.URL.Path) {
			notFound(w, r)
			return
		}

		// A file-only folder (sites/cdn) answers files, never a directory:
		// no index page and no listing.
		if !withIndex && strings.HasSuffix(r.URL.Path, "/") {
			notFound(w, r)
			return
		}

		if _, err := fs.Stat(root, strings.TrimPrefix(assetName(r.URL.Path), "/")); err != nil {
			notFound(w, r)
			return
		}

		if tag, ok := etags[assetName(r.URL.Path)]; ok {
			w.Header().Set("ETag", tag)
			w.Header().Set("Cache-Control", "no-cache")
		}

		files.ServeHTTP(w, r)
	})
}

// hasDotSegment reports whether any path segment starts with a dot.
func hasDotSegment(p string) bool {
	for _, segment := range strings.Split(p, "/") {
		if strings.HasPrefix(segment, ".") {
			return true
		}
	}

	return false
}

// errorPage is a custom body for an HTTP status, loaded from sites/.err_<code>.
type errorPage struct {
	body []byte
	etag string
}

func (page errorPage) serve(w http.ResponseWriter, r *http.Request, code int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if page.etag != "" {
		w.Header().Set("ETag", page.etag)
	}
	w.WriteHeader(code)

	if r.Method != http.MethodHead {
		_, _ = w.Write(page.body)
	}
}

var errorDirPattern = regexp.MustCompile(`^\.err_(\d{3})$`)

// loadErrorPages reads sites/.err_<code>/{index,<code>}.html so the site can
// own its error pages without any code.
func loadErrorPages() map[int]errorPage {
	pages := make(map[int]errorPage)

	entries, err := fs.ReadDir(sitesFS, "sites")
	if err != nil {
		return pages
	}

	for _, entry := range entries {
		match := errorDirPattern.FindStringSubmatch(entry.Name())
		if match == nil || !entry.IsDir() {
			continue
		}

		code, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}

		body := readErrorPage("sites/"+entry.Name(), code)
		if body == nil {
			log.Printf("error page %s has no html", entry.Name())
			continue
		}

		sum := sha256.Sum256(body)
		pages[code] = errorPage{body: body, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
	}

	if len(pages) > 0 {
		codes := make([]int, 0, len(pages))
		for code := range pages {
			codes = append(codes, code)
		}
		sort.Ints(codes)
		log.Printf("error pages: %v", codes)
	}

	return pages
}

func readErrorPage(dir string, code int) []byte {
	names := []string{"index.html", strconv.Itoa(code) + ".html"}

	for _, name := range names {
		if data, err := fs.ReadFile(sitesFS, path.Join(dir, name)); err == nil {
			return data
		}
	}

	entries, err := fs.ReadDir(sitesFS, dir)
	if err != nil {
		return nil
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(path.Ext(entry.Name()), ".html") {
			continue
		}
		if data, err := fs.ReadFile(sitesFS, path.Join(dir, entry.Name())); err == nil {
			return data
		}
	}

	return nil
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
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
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
