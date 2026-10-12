package health_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/fstest"
	"time"

	health "github.com/Elagoht/collage-health"
	"github.com/Elagoht/collage/pkg/collage"
)

// buildReader reads a finished build, as elagoht/deploy does; with one
// registered, the build asks the handler for every file to capture its headers.
type buildReader struct{ files []collage.BuiltFile }

func (*buildReader) Name() string                             { return "test/buildreader" }
func (*buildReader) Version() string                          { return "0" }
func (*buildReader) Init(context.Context, collage.Host) error { return nil }
func (*buildReader) Shutdown(context.Context) error           { return nil }
func (b *buildReader) OnBuildFinished(_ context.Context, ev *collage.BuildFinishedEvent) error {
	b.files = ev.Files
	return nil
}

// A static export holds pages, never the plugin's endpoints, and the build's
// header capture is not a client: with the one slot taken by a request still in
// flight, every capture is still answered.
func TestExport_NoEndpoints(t *testing.T) {
	reader := &buildReader{}
	hold := newHolder()
	app, err := collage.New(&collage.Config{
		Server: collage.ServerConfig{Host: "localhost", Port: 3000},
		Logger: slog.New(slog.DiscardHandler),
		Template: collage.TemplateConfig{FS: fstest.MapFS{
			"t/p.html": {Data: []byte(`<main>home</main>`)},
		}, Root: "t"},
		Plugins: []collage.Plugin{health.New(health.Options{MaxInFlight: 1}), hold, reader},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/a", "/b"} {
		page := collage.NewPage("p"+path).WithContent(collage.NewFragment("home", "p.html").Build()).WithPath("en", path).Build()
		if err := app.RegisterPage(page); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	h := app.Handler()
	// The one slot, taken for the whole build.
	release := holdN(t, h, hold, "build", 1)
	expectShed(t, do(h, http.MethodGet, "/a"))

	dir := t.TempDir()
	report, err := collage.Build(context.Background(), app, collage.BuildOptions{OutDir: dir})
	release()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, f := range report.Findings {
		t.Errorf("finding: %s %s: %s", f.Rule, f.Path, f.Message)
	}

	for _, name := range []string{"healthz", "readyz", "healthz/index.html", "readyz/index.html"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s in the export: %v", name, err)
		}
	}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if n := d.Name(); n == "healthz" || n == "readyz" {
			t.Errorf("%s in the export", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	captured := 0
	for _, f := range reader.files {
		if !f.Captured {
			continue
		}
		captured++
		if f.Status != http.StatusOK {
			t.Errorf("%s: status %d, want 200: a capture was shed", f.Path, f.Status)
		}
	}
	if captured != 3 {
		t.Fatalf("%d pages captured, want 3", captured)
	}
}

// freePort is a port nothing listens on at the moment it is returned.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// Over a real server: during the drain readiness says draining, liveness stays
// ok and pages are still served; once Shutdown returns the port is closed.
func TestE2E_DrainOverRealServer(t *testing.T) {
	const drain = 2 * time.Second
	port := freePort(t)
	app, err := collage.New(&collage.Config{
		Server: collage.ServerConfig{Host: "127.0.0.1", Port: port, DrainDelay: drain},
		Logger: slog.New(slog.DiscardHandler),
		Template: collage.TemplateConfig{FS: fstest.MapFS{
			"t/p.html": {Data: []byte(`<main>home</main>`)},
		}, Root: "t"},
		Plugins: []collage.Plugin{health.New(health.Options{})},
	})
	if err != nil {
		t.Fatal(err)
	}
	home := collage.NewPage("home").WithContent(collage.NewFragment("home", "p.html").Build()).WithPath("en", "/").Build()
	if err := app.RegisterPage(home); err != nil {
		t.Fatal(err)
	}

	served := make(chan error, 1)
	go func() { served <- app.ListenAndServe() }()

	base := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	// No pooled connection: the drain turns keep-alives off, and a reused one
	// closing under a request would be a spurious failure.
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	get := func(path string) (int, string, error) {
		resp, err := client.Get(base + path)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body), err
	}

	// Up: readiness answers ok.
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body, err := get("/readyz")
		if err == nil && code == http.StatusOK && body == "ok" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server never got ready: %d %q %v", code, body, err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdown := make(chan error, 1)
	start := time.Now()
	go func() { shutdown <- app.Shutdown(ctx) }()

	// The drain has begun once readiness says so.
	for {
		code, body, err := get("/readyz")
		if err == nil && code == http.StatusServiceUnavailable && body == "draining" {
			break
		}
		if time.Since(start) > drain/2 {
			t.Fatalf("readiness during the drain: %d %q %v, want 503 draining", code, body, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if code, body, err := get("/"); err != nil || code != http.StatusOK || body != "<main>home</main>" {
		t.Fatalf("a page during the drain: %d %q %v, want 200", code, body, err)
	}
	// Liveness stays up: a drain is no reason to restart the process.
	if code, body, err := get("/healthz"); err != nil || code != http.StatusOK || body != "ok" {
		t.Fatalf("liveness during the drain: %d %q %v, want 200 ok", code, body, err)
	}
	if since := time.Since(start); since >= drain {
		t.Fatalf("the drain checks took %s, past the %s drain: they did not run within it", since, drain)
	}

	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	if took := time.Since(start); took < drain {
		t.Errorf("Shutdown returned after %s, before the %s drain", took, drain)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("ListenAndServe: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return")
	}
	if conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("the port still accepts connections after Shutdown")
	}
}
