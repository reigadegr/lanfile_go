package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

const testTimeout = 5 * time.Second

func newTestApp(root string) *fiber.App {
	return newApp(root, 8000)
}

// do 发一个测试请求；header 为附加请求头。
func do(t *testing.T, app *fiber.App, method, target string, header map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for key, value := range header {
		req.Header.Set(key, value)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout, FailOnTimeout: true})
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func get(t *testing.T, app *fiber.App, target string, header ...map[string]string) *http.Response {
	t.Helper()
	var extra map[string]string
	if len(header) > 0 {
		extra = header[0]
	}
	return do(t, app, http.MethodGet, target, extra)
}

func body(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return data
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func requireStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d", resp.StatusCode, want)
	}
}

type listPayload struct {
	Path    string  `json:"path"`
	LanIP   *string `json:"lan_ip"`
	Port    uint16  `json:"port"`
	Entries []struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Size     *int64 `json:"size"`
		Modified string `json:"modified"`
	} `json:"entries"`
}

func decodeList(t *testing.T, resp *http.Response) listPayload {
	t.Helper()
	var payload listPayload
	if err := json.Unmarshal(body(t, resp), &payload); err != nil {
		t.Fatalf("decode list json: %v", err)
	}
	return payload
}

func entryByName(payload listPayload, name string) (int, bool) {
	for i, entry := range payload.Entries {
		if entry.Name == name {
			return i, true
		}
	}
	return 0, false
}

// ---- JSON API ----

func TestAPIListReturnsJSONForRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "abc")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	payload := decodeList(t, get(t, newTestApp(root), "/api/list"))
	if payload.Path != "/" {
		t.Errorf("path = %q, want /", payload.Path)
	}
	if payload.Port != 8000 {
		t.Errorf("port = %d, want 8000", payload.Port)
	}
	if len(payload.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(payload.Entries))
	}

	index, ok := entryByName(payload, "a.txt")
	if !ok {
		t.Fatal("a.txt missing")
	}
	file := payload.Entries[index]
	if file.Type != "file" {
		t.Errorf("a.txt type = %q, want file", file.Type)
	}
	if file.Size == nil || *file.Size != 3 {
		t.Errorf("a.txt size = %v, want 3", file.Size)
	}
	if file.Modified == "" {
		t.Error("a.txt modified is empty")
	}

	index, ok = entryByName(payload, "sub")
	if !ok {
		t.Fatal("sub missing")
	}
	dir := payload.Entries[index]
	if dir.Type != "dir" {
		t.Errorf("sub type = %q, want dir", dir.Type)
	}
	if dir.Size != nil {
		t.Errorf("sub size = %v, want null", *dir.Size)
	}
}

func TestAPIListSortsDirectoriesFirst(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "zzz.txt", "x")
	if err := os.MkdirAll(filepath.Join(root, "aaa"), 0o755); err != nil {
		t.Fatal(err)
	}

	payload := decodeList(t, get(t, newTestApp(root), "/api/list"))
	if len(payload.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(payload.Entries))
	}
	if payload.Entries[0].Name != "aaa" || payload.Entries[1].Name != "zzz.txt" {
		t.Errorf("order = %q, %q; want aaa, zzz.txt", payload.Entries[0].Name, payload.Entries[1].Name)
	}
}

func TestAPIListShowsDotFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".hidden", "secret")
	writeFile(t, root, "visible.txt", "abc")

	payload := decodeList(t, get(t, newTestApp(root), "/api/list"))
	if len(payload.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(payload.Entries))
	}
	if _, ok := entryByName(payload, ".hidden"); !ok {
		t.Error(".hidden missing")
	}
	if _, ok := entryByName(payload, "visible.txt"); !ok {
		t.Error("visible.txt missing")
	}
}

func TestAPIListHidesSymlinks(t *testing.T) {
	requireUnix(t)
	root := t.TempDir()
	writeFile(t, root, "real.txt", "real")
	if err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}

	resp := get(t, newTestApp(root), "/api/list")
	raw := string(body(t, resp))
	if strings.Contains(raw, "alias.txt") {
		t.Errorf("symlink leaked into listing: %s", raw)
	}

	payload := decodeList(t, get(t, newTestApp(root), "/api/list"))
	if len(payload.Entries) != 1 || payload.Entries[0].Name != "real.txt" {
		t.Errorf("entries = %+v, want only real.txt", payload.Entries)
	}
}

func TestAPIListListsSubdirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "sub/inner.txt", "xyz")

	payload := decodeList(t, get(t, newTestApp(root), "/api/list/sub"))
	if payload.Path != "/sub" {
		t.Errorf("path = %q, want /sub", payload.Path)
	}
	if len(payload.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(payload.Entries))
	}
	if payload.Entries[0].Name != "inner.txt" || payload.Entries[0].Type != "file" {
		t.Errorf("entry = %+v, want inner.txt file", payload.Entries[0])
	}
}

func TestAPIListReturns404ForMissingDirectory(t *testing.T) {
	requireStatus(t, get(t, newTestApp(t.TempDir()), "/api/list/nope"), http.StatusNotFound)
}

func TestAPIListReturns404ForFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "abc")
	requireStatus(t, get(t, newTestApp(root), "/api/list/a.txt"), http.StatusNotFound)
}

func TestAPIListRejectsPathTraversal(t *testing.T) {
	requireStatus(t, get(t, newTestApp(t.TempDir()), "/api/list/%2e%2e"), http.StatusNotFound)
}

// ---- File download ----

func TestFilesEndpointServesFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "hello.txt", "hello world")

	resp := get(t, newTestApp(root), "/files/hello.txt")
	requireStatus(t, resp, http.StatusOK)
	if got := string(body(t, resp)); got != "hello world" {
		t.Errorf("body = %q, want hello world", got)
	}
}

func TestFilesEndpointReturns404ForMissingFile(t *testing.T) {
	requireStatus(t, get(t, newTestApp(t.TempDir()), "/files/nope.txt"), http.StatusNotFound)
}

func TestFilesEndpointServesDotFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".hidden", "secret")

	resp := get(t, newTestApp(root), "/files/.hidden")
	requireStatus(t, resp, http.StatusOK)
	if got := string(body(t, resp)); got != "secret" {
		t.Errorf("body = %q, want secret", got)
	}
}

func TestFilesEndpointServesHiddenDirMember(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".git/config", "secret-config")

	resp := get(t, newTestApp(root), "/files/.git/config")
	requireStatus(t, resp, http.StatusOK)
	if got := string(body(t, resp)); got != "secret-config" {
		t.Errorf("body = %q, want secret-config", got)
	}
}

func TestFilesEndpointServesSpecialFilenames(t *testing.T) {
	// 文件名里的空格、# ? % 都必须能取到：Fiber 内部会把路径当 URI 传给文件处理器
	for name, want := range map[string]string{
		"a b.txt":  "space",
		"a#b.txt":  "hash",
		"q?.txt":   "question",
		"100%.txt": "percent",
		"a+b.txt":  "plus",
		"中文.txt":   "utf8",
	} {
		root := t.TempDir()
		writeFile(t, root, name, want)

		resp := get(t, newTestApp(root), "/files/"+url.PathEscape(name))
		requireStatus(t, resp, http.StatusOK)
		if got := string(body(t, resp)); got != want {
			t.Errorf("%q: body = %q, want %q", name, got, want)
		}
	}
}

func TestFilesEndpointReturns404ForDirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "sub/inner.txt", "xyz")
	requireStatus(t, get(t, newTestApp(root), "/files/sub"), http.StatusNotFound)
}

func TestFilesEndpointRejectsSymlink(t *testing.T) {
	requireUnix(t)
	root := t.TempDir()
	writeFile(t, root, "real.txt", "real")
	if err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, get(t, newTestApp(root), "/files/alias.txt"), http.StatusNotFound)
}

func TestFilesEndpointRejectsSymlinkEscape(t *testing.T) {
	requireUnix(t)
	outside := t.TempDir()
	writeFile(t, outside, "secret.txt", "top secret")
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, get(t, newTestApp(root), "/files/link.txt"), http.StatusNotFound)
}

func TestFilesEndpointRejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "secret.txt", "top secret")
	app := newTestApp(root)
	for _, target := range []string{
		"/files/%2e%2e%2f%2e%2e%2fetc%2fpasswd",
		"/files/../../etc/passwd",
		"/files/../secret.txt",
	} {
		requireStatus(t, get(t, app, target), http.StatusNotFound)
	}
}

func TestFilesEndpointHeadRequestSucceeds(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "hello.txt", "hello world")

	resp := do(t, newTestApp(root), http.MethodHead, "/files/hello.txt", nil)
	requireStatus(t, resp, http.StatusOK)
	if got := body(t, resp); len(got) != 0 {
		t.Errorf("HEAD body = %q, want empty", got)
	}
}

func TestFilesEndpointSupportsRangeRequests(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "hello.txt", "hello world")

	resp := get(t, newTestApp(root), "/files/hello.txt", map[string]string{"Range": "bytes=0-4"})
	requireStatus(t, resp, http.StatusPartialContent)
	if got := string(body(t, resp)); got != "hello" {
		t.Errorf("body = %q, want hello", got)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 0-4/11" {
		t.Errorf("Content-Range = %q, want bytes 0-4/11", got)
	}
}

func TestFilesEndpointSendsETagAndHonoursIfNoneMatch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "hello.txt", "hello world")
	app := newTestApp(root)

	resp := get(t, app, "/files/hello.txt")
	requireStatus(t, resp, http.StatusOK)
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("ETag header missing")
	}

	notModified := get(t, app, "/files/hello.txt", map[string]string{"If-None-Match": etag})
	requireStatus(t, notModified, http.StatusNotModified)

	stale := get(t, app, "/files/hello.txt", map[string]string{"If-None-Match": `"nope"`})
	requireStatus(t, stale, http.StatusOK)
}

// ---- Real TCP ----

func TestServesOverRealTCP(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "hello.txt", "hello world")

	app := newTestApp(root)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { _ = app.Shutdown() })

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	_, err = fmt.Fprintf(conn, "GET /files/hello.txt HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
	if err != nil {
		t.Fatalf("write request: %v", err)
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	text := string(raw)
	if !strings.HasPrefix(text, "HTTP/1.1 200") {
		t.Errorf("response: %s", text)
	}
	if !strings.Contains(text, "hello world") {
		t.Errorf("response missing body: %s", text)
	}
}

// ---- Frontend page ----

func TestRootReturnsHTMLPage(t *testing.T) {
	resp := get(t, newTestApp(t.TempDir()), "/")
	requireStatus(t, resp, http.StatusOK)

	page := string(body(t, resp))
	for _, want := range []string{"<title>文件浏览</title>", "/static/style.css", "/static/app.js"} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
}

func TestStaticServesCSS(t *testing.T) {
	resp := get(t, newTestApp(t.TempDir()), "/static/style.css")
	requireStatus(t, resp, http.StatusOK)
	if !strings.Contains(string(body(t, resp)), "border-box") {
		t.Error("css body missing border-box")
	}
}

func TestStaticFallsBackToIndex(t *testing.T) {
	resp := get(t, newTestApp(t.TempDir()), "/static/nope.css")
	requireStatus(t, resp, http.StatusOK)
	if !strings.Contains(string(body(t, resp)), "<title>文件浏览</title>") {
		t.Error("fallback did not serve index.html")
	}
}

func TestUnknownPathReturns404(t *testing.T) {
	requireStatus(t, get(t, newTestApp(t.TempDir()), "/nope"), http.StatusNotFound)
}

// ---- Zip download ----

func readZip(t *testing.T, data []byte) map[string]*zip.File {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("parse zip: %v", err)
	}
	entries := make(map[string]*zip.File, len(reader.File))
	for _, file := range reader.File {
		entries[file.Name] = file
	}
	return entries
}

func TestAPIZipStreamsFolder(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "hello")
	writeFile(t, root, "sub/b.txt", "world")

	resp := get(t, newTestApp(root), "/api/zip")
	requireStatus(t, resp, http.StatusOK)
	if got := resp.Header.Get("Content-Type"); got != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", got)
	}
	if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment; filename*=UTF-8''") {
		t.Errorf("Content-Disposition = %q", got)
	}

	entries := readZip(t, body(t, resp))
	folder := filepath.Base(root)
	for _, want := range []string{folder + "/", folder + "/a.txt", folder + "/sub/", folder + "/sub/b.txt"} {
		if _, ok := entries[want]; !ok {
			t.Errorf("zip missing %q (have %v)", want, keys(entries))
		}
	}
}

func TestAPIZipReturns404ForMissing(t *testing.T) {
	requireStatus(t, get(t, newTestApp(t.TempDir()), "/api/zip/nope"), http.StatusNotFound)
}

func TestAPIZipReturns404ForFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "abc")
	requireStatus(t, get(t, newTestApp(root), "/api/zip/a.txt"), http.StatusNotFound)
}

func TestAPIZipRejectsPathTraversal(t *testing.T) {
	requireStatus(t, get(t, newTestApp(t.TempDir()), "/api/zip/%2e%2e"), http.StatusNotFound)
}

func TestAPIZipIncludesDotFilesAndDirs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".hidden", "secret")
	writeFile(t, root, ".git/config", "cfg")

	entries := readZip(t, body(t, get(t, newTestApp(root), "/api/zip")))
	folder := filepath.Base(root)
	for _, want := range []string{folder + "/.hidden", folder + "/.git/", folder + "/.git/config"} {
		if _, ok := entries[want]; !ok {
			t.Errorf("zip missing %q (have %v)", want, keys(entries))
		}
	}
}

func TestAPIZipPreservesEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	entries := readZip(t, body(t, get(t, newTestApp(root), "/api/zip")))
	folder := filepath.Base(root)
	if _, ok := entries[folder+"/empty/"]; !ok {
		t.Errorf("zip missing empty dir (have %v)", keys(entries))
	}
}

func TestAPIZipSkipsSymlinks(t *testing.T) {
	requireUnix(t)
	root := t.TempDir()
	writeFile(t, root, "real.txt", "real")
	if err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}

	entries := readZip(t, body(t, get(t, newTestApp(root), "/api/zip")))
	for name := range entries {
		if strings.HasSuffix(name, "alias.txt") {
			t.Errorf("symlink leaked into zip: %v", keys(entries))
		}
	}
}

func TestAPIZipSkipsUnreadableDirectory(t *testing.T) {
	requireUnix(t)
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root := t.TempDir()
	writeFile(t, root, "locked/secret.txt", "secret")
	if err := os.Chmod(filepath.Join(root, "locked"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "locked"), 0o755) })

	resp := get(t, newTestApp(root), "/api/zip")
	requireStatus(t, resp, http.StatusOK)
	readZip(t, body(t, resp))
}

func TestAPIZipPreservesFileContent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "hello")

	entries := readZip(t, body(t, get(t, newTestApp(root), "/api/zip")))
	file, ok := entries[filepath.Base(root)+"/a.txt"]
	if !ok {
		t.Fatalf("zip missing a.txt (have %v)", keys(entries))
	}
	reader, err := file.Open()
	if err != nil {
		t.Fatalf("open zip entry: %v", err)
	}
	defer reader.Close()
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read zip entry: %v", err)
	}
	if string(content) != "hello" {
		t.Errorf("entry content = %q, want hello", content)
	}
	if !file.Mode().IsRegular() {
		t.Errorf("entry mode = %v, want regular file", file.Mode())
	}
}

// ---- helpers ----

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix-only behaviour")
	}
}

// ---- CLI ----

func TestParseArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		port uint16
		dir  string
	}{
		{"defaults", nil, 8000, "."},
		{"path only", []string{"/srv/www"}, 8000, "/srv/www"},
		{"port only", []string{"8080"}, 8080, "."},
		{"both", []string{"8080", "/srv/www"}, 8080, "/srv/www"},
		{"order irrelevant", []string{"/srv/www", "8080"}, 8080, "/srv/www"},
		{"last path wins", []string{"/srv/www", "/data"}, 8000, "/data"},
		{"last port wins", []string{"8080", "9000"}, 9000, "."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port, dir := parseArgs(tc.args)
			if port != tc.port || dir != tc.dir {
				t.Errorf("parseArgs(%v) = (%d, %q), want (%d, %q)", tc.args, port, dir, tc.port, tc.dir)
			}
		})
	}
}

func TestWithinRoot(t *testing.T) {
	for _, tc := range []struct {
		root, path string
		want       bool
	}{
		{"/srv/www", "/srv/www/a.txt", true},
		{"/srv/www", "/srv/www/sub/a.txt", true},
		{"/srv/www", "/srv/www", true},
		{"/srv/www", "/srv/wwwroot/a.txt", false},
		{"/srv/www", "/srv", false},
		{"/srv/www", "/etc/passwd", false},
	} {
		if got := withinRoot(tc.root, tc.path); got != tc.want {
			t.Errorf("withinRoot(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
		}
	}
}

func TestContentDisposition(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, want string
	}{
		{"a.txt", "text/plain; charset=utf-8", "inline"},
		{"a.html", "text/html; charset=utf-8", "inline"},
		{"a.png", "image/png", "inline"},
		{"a.mp4", "video/mp4", "inline"},
		{"a.json", "application/json", "inline"},
		{"a.svg", "image/svg+xml", `attachment; filename="a.svg"`},
		{"a.xml", "text/xml", `attachment; filename="a.xml"`},
		{"a.bin", "application/octet-stream", `attachment; filename="a.bin"`},
		{"中文.bin", "application/octet-stream", `attachment; filename="__.bin"; filename*=UTF-8''%E4%B8%AD%E6%96%87.bin`},
	} {
		if got := contentDisposition(tc.name, tc.contentType); got != tc.want {
			t.Errorf("contentDisposition(%q, %q) = %q, want %q", tc.name, tc.contentType, got, tc.want)
		}
	}
}

func TestMatchesIfNoneMatch(t *testing.T) {
	for _, tc := range []struct {
		header, etag string
		want         bool
	}{
		{"", `"abc"`, false},
		{"*", `"abc"`, true},
		{`"abc"`, `"abc"`, true},
		{`W/"abc"`, `"abc"`, true},
		{`"other", "abc"`, `"abc"`, true},
		{`"other"`, `"abc"`, false},
	} {
		if got := matchesIfNoneMatch(tc.header, tc.etag); got != tc.want {
			t.Errorf("matchesIfNoneMatch(%q, %q) = %v, want %v", tc.header, tc.etag, got, tc.want)
		}
	}
}

func TestFolderName(t *testing.T) {
	for _, tc := range []struct{ dir, want string }{
		{"/", "root"},
		{"/srv/www", "www"},
		{"/srv/www/", "www"},
	} {
		if got := folderName(tc.dir); got != tc.want {
			t.Errorf("folderName(%q) = %q, want %q", tc.dir, got, tc.want)
		}
	}
}
