package main

import (
	"errors"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// newApp 组装应用：访问日志 → 文件下载 → 列表/打包 API → 内嵌前端资源。
func newApp(root string, port uint16) *fiber.App {
	app := fiber.New()
	app.Use(accessLog)
	registerFiles(app, root)
	registerList(app, root, port)
	registerZip(app, root)
	registerStatic(app)
	return app
}

// accessLog 在响应结束后记录来源 IP、方法、路径、协议版本、状态码与响应长度。
func accessLog(c fiber.Ctx) error {
	err := c.Next()

	size := "-"
	if length := c.Response().Header.Peek(fiber.HeaderContentLength); len(length) > 0 {
		size = string(length)
	}
	logf(
		"INFO access: ip=%s method=%s path=%s version=%s status=%d size=%s",
		c.IP(), c.Method(), c.Path(), c.Protocol(), statusOf(c, err), size,
	)
	return err
}

// statusOf 取响应状态码。处理器返回错误时状态码还没被 Fiber 的错误处理器写进响应，
// 所以直接从错误推导，避免把 404 记成 200。
func statusOf(c fiber.Ctx, err error) int {
	if err == nil {
		return c.Response().StatusCode()
	}
	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		return fiberErr.Code
	}
	return fiber.StatusInternalServerError
}

// wildcard 取路由尾部通配参数并解码百分号转义。
// Fiber 默认把路径原样交给路由，文件名里的空格等字符会带着 %XX 进来，这里还原成真实文件名；
// 非法转义（文件名里可能真的含 %）原样保留。
func wildcard(c fiber.Ctx) string {
	raw := c.Params("*")
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return raw
	}
	return decoded
}

// withinRoot 判断 path 是否位于 root 之内（按路径分量比较，避免 /srv/wwwroot 误判为 /srv/www 的子路径）。
func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
