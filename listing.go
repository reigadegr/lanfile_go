package main

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/gofiber/fiber/v3"
)

// listEntry 是 /api/list 返回的单条目录项；目录没有大小，size 为 null。
type listEntry struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Size     *int64 `json:"size"`
	Modified string `json:"modified"`
}

// listResponse 是 /api/list 的响应体。
type listResponse struct {
	Path    string      `json:"path"`
	LanIP   *string     `json:"lan_ip"`
	Port    uint16      `json:"port"`
	Entries []listEntry `json:"entries"`
}

func registerList(app *fiber.App, root string, port uint16) {
	cache := &lanIPCache{}

	handler := func(c fiber.Ctx) error {
		sub := wildcard(c)
		entries, ok := listDirectory(root, sub)
		if !ok {
			return fiber.ErrNotFound
		}

		displayPath := "/"
		if sub != "" {
			displayPath = "/" + sub
		}
		var lanIP *string
		if ip := cache.get(); ip != "" {
			lanIP = &ip
		}

		return c.JSON(listResponse{
			Path:    displayPath,
			LanIP:   lanIP,
			Port:    port,
			Entries: entries,
		})
	}

	app.Get("/api/list", handler)
	app.Get("/api/list/*", handler)
}

// resolveDir 解析请求路径对应的目录，且必须位于 root 之内（防目录穿越）。
func resolveDir(root, sub string) (string, bool) {
	canonical, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(sub)))
	if err != nil || !withinRoot(root, canonical) {
		return "", false
	}
	return canonical, true
}

// listDirectory 枚举目录并返回排序后的条目；路径非法或不是目录时返回 false。
func listDirectory(root, sub string) ([]listEntry, bool) {
	dir, ok := resolveDir(root, sub)
	if !ok {
		return nil, false
	}
	// os.ReadDir 已按文件名排序；对文件调用时返回 ENOTDIR，正好对应 404
	dirents, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}

	entries := make([]listEntry, 0, len(dirents))
	for _, dirent := range dirents {
		// Info 对符号链接返回链接自身的元信息，据此把符号链接排除在列表外
		info, err := dirent.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}

		entry := listEntry{
			Name:     dirent.Name(),
			Type:     "file",
			Modified: info.ModTime().UTC().Format("2006-01-02T15:04:05"),
		}
		if info.IsDir() {
			entry.Type = "dir"
		} else {
			size := info.Size()
			entry.Size = &size
		}
		entries = append(entries, entry)
	}

	sortListEntries(entries)
	return entries, true
}

// sortListEntries 目录在前，同类按名称升序。
func sortListEntries(entries []listEntry) {
	sort.Slice(entries, func(i, j int) bool {
		leftDir := entries[i].Type == "dir"
		rightDir := entries[j].Type == "dir"
		if leftDir != rightDir {
			return leftDir
		}
		return entries[i].Name < entries[j].Name
	})
}
