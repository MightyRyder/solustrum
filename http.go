package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
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

// Every directory under sites/ is compiled into the binary, so a single
// artifact can be shipped to AWS with no side files. all: is required because
// .type.conf is a dotfile and _errors/ is underscore-prefixed; both are
// embedded but never served as sites.
//
//go:embed all:sites
var sitesFS embed.FS

// typeConfName overrides how a folder is mounted. Content is either
// "type subdomain" or "type prefix" (bare "subdomain"/"prefix" also works).
// Missing file: top-level folders are subdomains, nested folders are prefixes.
const typeConfName = ".type.conf"

// errorDirName holds custom error pages. sites/_errors/404 maps directly to
// /_errors/404/, so the source path and URL stay identical. The underscore
// also keeps it out of site discovery.
const errorDirName = "_errors"

// devCookieName opts a browser into the dev deployment. The stable front
// proxies matching requests to DEV_URL; without it, requests stay on stable.
const devCookieName = "solustrum_dev"

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

	// plainNotFound is used inside /_errors/<code>/, where serving the error
	// page again would recurse.
	plainNotFound := http.NotFound
	notFound := func(w http.ResponseWriter, r *http.Request) {
		// Serve the page inline at the URL that failed, so the address bar
		// keeps the requested path. Its assets are absolute (/_errors/<code>/),
		// so they resolve on every host and at every depth.
		if page, ok := errorPages[http.StatusNotFound]; ok {
			page.serve(w, r, http.StatusNotFound)
			return
		}

		plainNotFound(w, r)
	}

	sites, err := loadSites(notFound)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/hello", helloHandler)
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/_dev", devHandler)

	// The Cloudflare tunnel points straight at this process, so the deploy
	// webhook has to be forwarded to its own listener. Unset = disabled.
	if target := os.Getenv("WEBHOOK_URL"); target != "" {
		u, err := url.Parse(target)
		if err != nil {
			return nil, fmt.Errorf("parse WEBHOOK_URL: %w", err)
		}
		mux.Handle("/deploy/solustrum", httputil.NewSingleHostReverseProxy(u))
	}

	// Error pages live at /_errors/<code>/: the folder's index.html is the page
	// and the rest of the folder serves its assets. Mounted globally, so a
	// 404 looks the same on every host, independent of the site that failed.
	for code, page := range errorPages {
		prefix := "/_errors/" + strconv.Itoa(code) + "/"
		page, code := page, code
		assets := staticHandler(page.fsys, false, false, plainNotFound)

		mux.Handle(prefix, http.StripPrefix(prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// StripPrefix turns the mount root into "" (or "/" via a
			// browser-normalized request); both mean "serve the page".
			if r.URL.Path == "" || r.URL.Path == "/" {
				page.serve(w, r, code)

				return
			}

			assets.ServeHTTP(w, r)
		})))
	}

	mux.Handle("/", siteRouter(sites, notFound))

	handler := http.Handler(mux)
	if version := os.Getenv("VERSION"); version != "" {
		handler = versionHeader(handler, version)
	}
	if devURL := os.Getenv("DEV_URL"); devURL != "" {
		dev, err := devRouter(handler, devURL)
		if err != nil {
			return nil, err
		}
		handler = dev
	}

	return handler, nil
}

// devHandler toggles the preview cookie. /_dev?on=1 enables it,
// /_dev?off=1 clears it, and /_dev alone reports the current state.
func devHandler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	switch {
	case query.Get("on") == "1":
		setDevCookie(w, r, true)
		http.Redirect(w, r, "/", http.StatusFound)

		return
	case query.Get("off") == "1", query.Get("on") == "0", query.Get("on") == "off":
		setDevCookie(w, r, false)
		http.Redirect(w, r, "/", http.StatusFound)

		return
	}

	state := "disabled"
	if wantsDev(r) {
		state = "enabled"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!doctype html><meta name="viewport" content="width=device-width, initial-scale=1"><title>dev preview</title><h1>dev preview: %s</h1><p><a href="/_dev?on=1">enable</a> &middot; <a href="/_dev?off=1">disable</a></p>`, state)
}

func setDevCookie(w http.ResponseWriter, r *http.Request, on bool) {
	cookie := &http.Cookie{
		Name:     devCookieName,
		Value:    "1",
		Path:     "/",
		Domain:   devCookieDomain(r.Host),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
	}
	if on {
		cookie.MaxAge = 30 * 24 * 60 * 60
	} else {
		cookie.Value = ""
		cookie.MaxAge = -1
	}

	http.SetCookie(w, cookie)
}

// devCookieDomain scopes the preview cookie to every solustrum.com host, so
// assets.solustrum.com and foo.solustrum.com switch along with the apex.
func devCookieDomain(host string) string {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")

	base := os.Getenv("BASE_DOMAIN")
	if base == "" {
		base = defaultBaseDomain
	}
	base = strings.ToLower(strings.TrimSuffix(base, "."))

	if host == base || strings.HasSuffix(host, "."+base) {
		return "." + base
	}

	return ""
}

func wantsDev(r *http.Request) bool {
	cookie, err := r.Cookie(devCookieName)

	return err == nil && cookie.Value == "1"
}

// devRouter proxies preview requests to the dev process. Everything except the
// toggle, health, hello, and deploy paths is eligible, including error-page
// assets, so a dev page stays internally consistent.
func devRouter(stable http.Handler, target string) (http.Handler, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("parse DEV_URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("DEV_URL must be absolute, got %q", target)
	}

	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "dev preview unavailable; disable it at /_dev?off=1", http.StatusBadGateway)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !wantsDev(r) || isLocalPath(r.URL.Path) {
			stable.ServeHTTP(w, r)

			return
		}

		proxy.ServeHTTP(w, r)
	}), nil
}

func isLocalPath(p string) bool {
	return p == "/_dev" || strings.HasPrefix(p, "/_dev/") ||
		p == "/healthz" || p == "/hello" ||
		strings.HasPrefix(p, "/deploy/")
}

func versionHeader(next http.Handler, version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Solustrum-Version", version)
		next.ServeHTTP(w, r)
	})
}

// folder is one directory in sites/ and how it mounts. A subdomain folder
// owns a hostname (and a handler); a prefix folder is just a path inside its
// nearest subdomain ancestor.
type folder struct {
	name string
	rel  string // slash path relative to sites/, e.g. "foo/bar"
	kind string // "subdomain" or "prefix"
	host string // subdomain label chain, e.g. "bar.foo"; empty for sites/root
	kids []*folder
}

// loadSites walks sites/ and builds one handler per subdomain folder. Hidden
// and underscore-prefixed folders, plus the _errors/ error-page tree, are
// skipped. A subdomain folder is hidden from its parent's file tree.
func loadSites(notFound http.HandlerFunc) (map[string]http.Handler, error) {
	entries, err := fs.ReadDir(sitesFS, "sites")
	if err != nil {
		return nil, fmt.Errorf("read sites: %w", err)
	}

	handlers := make(map[string]http.Handler)
	var trees []*folder
	var skipped []string

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		if name == errorDirName {
			skipped = append(skipped, name+" (error pages)")
			continue
		}
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			skipped = append(skipped, name+" (hidden)")
			continue
		}

		tree, err := scanFolder(name, "", 1)
		if err != nil {
			return nil, err
		}
		trees = append(trees, tree)
	}

	var accepted []string
	for _, tree := range trees {
		if err := registerSites(tree, handlers, notFound, &accepted); err != nil {
			return nil, err
		}
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

// scanFolder reads one folder's mount type and recurses. parentHost is the
// host of the nearest subdomain ancestor ("" for sites/root and the apex).
// Missing .type.conf means subdomain at depth 1 and prefix below it.
func scanFolder(rel, parentHost string, depth int) (*folder, error) {
	name := path.Base(rel)
	kind := readTypeConf(rel)

	if name == "root" && depth == 1 {
		kind = "subdomain"
	}
	if kind == "" {
		if depth == 1 {
			kind = "subdomain"
		} else {
			kind = "prefix"
		}
	}
	if depth == 1 && kind == "prefix" {
		return nil, fmt.Errorf("sites/%s: type prefix at the top level has no parent site", rel)
	}

	f := &folder{name: name, rel: rel, kind: kind}
	switch {
	case name == "root" && depth == 1:
		f.host = "" // the apex
	case kind == "subdomain" && parentHost == "":
		f.host = name
	case kind == "subdomain":
		f.host = name + "." + parentHost
	default:
		f.host = parentHost
	}

	entries, err := fs.ReadDir(sitesFS, "sites/"+rel)
	if err != nil {
		return nil, fmt.Errorf("read sites/%s: %w", rel, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		child := entry.Name()
		if strings.HasPrefix(child, ".") || strings.HasPrefix(child, "_") {
			continue
		}

		kid, err := scanFolder(path.Join(rel, child), f.host, depth+1)
		if err != nil {
			return nil, err
		}
		f.kids = append(f.kids, kid)
	}

	return f, nil
}

// readTypeConf returns the declared mount type, or "" when the file is absent
// or says something unrecognized (the caller then applies the default).
func readTypeConf(rel string) string {
	data, err := fs.ReadFile(sitesFS, path.Join("sites", rel, typeConfName))
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.ToLower(line))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		for _, word := range strings.FieldsFunc(line, func(r rune) bool {
			return r < 'a' || r > 'z'
		}) {
			if word == "subdomain" || word == "prefix" {
				return word
			}
		}

		log.Printf("sites/%s/%s: expected subdomain or prefix, got %q", rel, typeConfName, line)

		return ""
	}

	return ""
}

// registerSites creates a handler for every subdomain folder in the tree.
// Prefix folders are not registered: their nearest subdomain ancestor serves
// them as a path, and any subdomain below them is registered in its own right.
func registerSites(f *folder, handlers map[string]http.Handler, notFound http.HandlerFunc, accepted *[]string) error {
	if f.kind == "subdomain" {
		key := f.host
		if f.rel == "root" {
			key = "root"
		}
		if _, exists := handlers[key]; exists {
			return fmt.Errorf("sites/%s: host %q is already used", f.rel, key)
		}
		if key == "root" && f.rel != "root" {
			return fmt.Errorf("sites/%s: host %q is reserved for the apex", f.rel, key)
		}

		root, err := fs.Sub(sitesFS, "sites/"+f.rel)
		if err != nil {
			return fmt.Errorf("sub sites/%s: %w", f.rel, err)
		}

		visible := hideSubdomains(root, f)
		withIndex := hasHTML(visible)
		handlers[key] = staticHandler(visible, !withIndex, true, notFound)

		siteKind := "site"
		if !withIndex {
			siteKind = "files"
		}
		where := "host " + key
		if f.rel == "root" {
			where = "apex"
		}
		*accepted = append(*accepted, fmt.Sprintf("%s (%s, %s)", f.rel, where, siteKind))
	}

	for _, kid := range f.kids {
		if err := registerSites(kid, handlers, notFound, accepted); err != nil {
			return err
		}
	}

	return nil
}

// hideSubdomains removes every nested subdomain folder from a handler's tree,
// so foo.solustrum.com cannot serve sites/foo/bar once bar has its own host.
// Prefix folders are walked through; the first subdomain stops the descent
// because that folder owns its own handler and its own hidden set.
func hideSubdomains(root fs.FS, f *folder) fs.FS {
	var hidden []string

	var walk func(n *folder)
	walk = func(n *folder) {
		for _, kid := range n.kids {
			if kid.kind == "subdomain" {
				hidden = append(hidden, strings.TrimPrefix(kid.rel, f.rel+"/"))
				continue
			}
			walk(kid)
		}
	}
	walk(f)

	if len(hidden) == 0 {
		return root
	}

	return newHideFS(root, hidden)
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

// siteRouter maps the request host to the folder that owns it. Hosts answer
// only on their own name; there is no apex path mirror.
func siteRouter(sites map[string]http.Handler, notFound http.HandlerFunc) http.Handler {
	base := os.Getenv("BASE_DOMAIN")
	if base == "" {
		base = defaultBaseDomain
	}
	base = strings.ToLower(strings.TrimSuffix(base, "."))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler, ok := siteFor(r.Host, base, sites)
		if !ok {
			notFound(w, r)
			return
		}

		handler.ServeHTTP(w, r)
	})
}

// siteFor maps the apex and <label>.<base> to a registered host. Nested
// subdomains match their full label chain, e.g. bar.foo.<base>.
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

	for _, suffix := range []string{"." + base, ".localhost"} {
		if key, found := strings.CutSuffix(host, suffix); found {
			// sites/root is the apex only; root.<base> is not a second copy.
			if key == "root" {
				return nil, false
			}

			handler, ok := sites[key]

			return handler, ok
		}
	}

	handler, ok := sites["root"]

	return handler, ok
}

// hideFS hides paths from an fs.FS. It keeps nested subdomain folders out of
// their parent's file server, directory listings, and hasHTML scan.
type hideFS struct {
	fsys   fs.FS
	hidden map[string]struct{}
}

func newHideFS(fsys fs.FS, hidden []string) fs.FS {
	set := make(map[string]struct{}, len(hidden))
	for _, p := range hidden {
		set[path.Clean(p)] = struct{}{}
	}

	return hideFS{fsys: fsys, hidden: set}
}

func (h hideFS) isHidden(name string) bool {
	name = path.Clean(name)
	if name == "." {
		return false
	}

	for hidden := range h.hidden {
		if name == hidden || strings.HasPrefix(name, hidden+"/") {
			return true
		}
	}

	return false
}

func (h hideFS) Open(name string) (fs.File, error) {
	if h.isHidden(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	f, err := h.fsys.Open(name)
	if err != nil {
		return nil, err
	}

	dir, ok := f.(fs.ReadDirFile)
	if !ok {
		return f, nil
	}

	return hideDir{ReadDirFile: dir, fsys: h, dir: path.Clean(name)}, nil
}

type hideDir struct {
	fs.ReadDirFile
	fsys hideFS
	dir  string
}

func (d hideDir) ReadDir(n int) ([]fs.DirEntry, error) {
	var out []fs.DirEntry

	for {
		entries, err := d.ReadDirFile.ReadDir(n)
		for _, entry := range entries {
			if !d.fsys.isHidden(path.Join(d.dir, entry.Name())) {
				out = append(out, entry)
			}
		}

		if n <= 0 {
			if errors.Is(err, io.EOF) {
				err = nil
			}

			return out, err
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(out) > 0 {
				return out, nil
			}

			return out, err
		}
		if len(out) >= n {
			return out, nil
		}
	}
}

// staticHandler serves one folder. In download mode (no html) files are sent
// as attachments while subdirectories stay browsable. Files carry a
// content-hash ETag and "no-cache", so caches revalidate instead of replaying
// stale bytes after a deploy.
func staticHandler(root fs.FS, download, list bool, notFound http.HandlerFunc) http.Handler {
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

		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "."
		}

		info, err := fs.Stat(root, name)
		if err != nil {
			notFound(w, r)
			return
		}

		if info.IsDir() && !list {
			notFound(w, r)
			return
		}

		if download && !info.IsDir() {
			if disposition := mime.FormatMediaType("attachment", map[string]string{
				"filename": path.Base(r.URL.Path),
			}); disposition != "" {
				w.Header().Set("Content-Disposition", disposition)
			}
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

// errorPage is a custom body for an HTTP status, loaded from sites/_errors/<code>.
type errorPage struct {
	fsys fs.FS
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

var errorCodePattern = regexp.MustCompile(`^(\d{3})$`)

// loadErrorPages reads sites/_errors/<code>/{index,<code>}.html so the site
// can own its error pages without any code.
func loadErrorPages() map[int]errorPage {
	pages := make(map[int]errorPage)
	root := path.Join("sites", errorDirName)

	entries, err := fs.ReadDir(sitesFS, root)
	if err != nil {
		return pages
	}

	for _, entry := range entries {
		match := errorCodePattern.FindStringSubmatch(entry.Name())
		if match == nil || !entry.IsDir() {
			continue
		}

		code, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}

		dir := path.Join(root, entry.Name())
		body := readErrorPage(dir, code)
		if body == nil {
			log.Printf("error page %s has no html", dir)
			continue
		}

		siteRoot, err := fs.Sub(sitesFS, dir)
		if err != nil {
			continue
		}

		sum := sha256.Sum256(body)
		pages[code] = errorPage{
			fsys: siteRoot,
			body: body,
			etag: `"` + hex.EncodeToString(sum[:8]) + `"`,
		}
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
