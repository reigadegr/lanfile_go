package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFileBodyCloseIsIdempotent 覆盖重复关闭正文流的情况：fasthttp 在正文写完后
// 与响应重置时都会调用 Close，第二次调用不得 panic，也不得重复归还引用。
func TestFileBodyCloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := newFileCache()
	entry, err := cache.store("/a.md", path, path)
	if err != nil {
		t.Fatal(err)
	}
	body := &fileBody{cache: cache, entry: entry, size: entry.size}
	if err := body.Close(); err != nil {
		t.Fatalf("首次关闭失败: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("第二次关闭应无害，实际返回: %v", err)
	}
	if entry.refs != 0 {
		t.Fatalf("重复关闭后引用计数应为 0，实际 %d", entry.refs)
	}
}
