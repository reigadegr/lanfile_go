package main

import (
	"archive/zip"
	"bufio"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/gofiber/fiber/v3"
)

// zipStreamBufferSize 是 zip 输出的大缓冲；fasthttp 给的 bufio.Writer 只有 4KB，
// 套一层大缓冲能让每次系统调用写出更大的块。
const zipStreamBufferSize = 64 * 1024

// zipEntry 是 zip 归档中的一条记录；dir 为真时是目录条目（用于保留空目录结构），此时 abs 无意义。
type zipEntry struct {
	name string
	abs  string
	dir  bool
}

func registerZip(app *fiber.App, root string) {
	handler := func(c fiber.Ctx) error {
		dir, ok := resolveDir(root, wildcard(c))
		if !ok {
			return fiber.ErrNotFound
		}
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			return fiber.ErrNotFound
		}

		folder := folderName(dir)
		c.Set(fiber.HeaderContentType, "application/zip")
		c.Set(fiber.HeaderContentDisposition, zipContentDisposition(folder))

		return c.SendStreamWriter(func(w *bufio.Writer) {
			writeZip(w, dir, folder)
		})
	}

	app.Get("/api/zip", handler)
	app.Get("/api/zip/*", handler)
}

// writeZip 边遍历边把目录树写成 zip 流，不先把整棵树攒进内存。
// 文件一律用 Store 原样存储（不压缩），目录条目以 / 结尾，解压后目录结构原样保留。
func writeZip(w *bufio.Writer, dir, folder string) {
	out := bufio.NewWriterSize(w, zipStreamBufferSize)
	zw := zip.NewWriter(out)
	defer func() {
		_ = zw.Close()
		_ = out.Flush()
	}()

	walk(dir, folder, func(entry zipEntry) bool {
		if entry.dir {
			header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
			header.SetMode(fs.ModeDir | 0o755)
			_, err := zw.CreateHeader(header)
			return err == nil
		}

		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		header.SetMode(0o644)
		dst, err := zw.CreateHeader(header)
		if err != nil {
			return false
		}

		src, err := os.Open(entry.abs)
		if err != nil {
			// 单个文件打不开就留个空条目继续，不中断整个打包
			return true
		}
		defer src.Close()

		_, err = io.Copy(dst, src)
		return err == nil
	})
}

// walk 深度优先遍历目录树，把每个条目交给 onEntry；onEntry 返回 false 时提前停止。
// 与 /api/list 一致：包含 dotfile、跳过符号链接；os.ReadDir 已按文件名排序，zip 内顺序因此确定。
// 目录不可读（无权限等）时跳过该目录，不中断整个打包。
func walk(dir, prefix string, onEntry func(zipEntry) bool) {
	// 先确认目录可读，读不到就不产出目录条目，避免解压时出现空目录
	dirents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	// 记录目录自身，空目录解压后也能保留
	if !onEntry(zipEntry{name: prefix + "/", dir: true}) {
		return
	}

	for _, dirent := range dirents {
		info, err := dirent.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(dir, dirent.Name())
		zipName := prefix + "/" + dirent.Name()

		switch {
		case info.IsDir():
			walk(path, zipName, onEntry)
		case info.Mode().IsRegular():
			if !onEntry(zipEntry{name: zipName, abs: path}) {
				return
			}
		}
		// 符号链接等其它类型：跳过
	}
}

// folderName 取目录名作为 zip 内根前缀（也用于 Content-Disposition 文件名）。
func folderName(dir string) string {
	name := filepath.Base(dir)
	if name == "." || name == string(filepath.Separator) {
		return "root"
	}
	return name
}

// zipContentDisposition 生成 RFC 5987 风格的 Content-Disposition，文件名是百分号编码后的目录名加 .zip。
func zipContentDisposition(folder string) string {
	return "attachment; filename*=UTF-8''" + percentEncode(folder, rfc5987AttrChar) + ".zip"
}
