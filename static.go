package main

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gofiber/fiber/v3"
)

//go:embed static
var staticAssets embed.FS

// registerStatic 提供内嵌前端资源：/ 返回首页，/static/{path} 取对应资源，
// 资源不存在时同样回落到首页。其余路径不匹配任何路由，直接 404。
func registerStatic(app *fiber.App) {
	assets, err := fs.Sub(staticAssets, "static")
	if err != nil {
		panic(err)
	}

	serve := func(c fiber.Ctx) error {
		name := strings.Trim(c.Params("*"), "/")
		if name == "" {
			name = "index.html"
		}
		data, err := fs.ReadFile(assets, name)
		if err != nil {
			name = "index.html"
			if data, err = fs.ReadFile(assets, name); err != nil {
				return fiber.ErrNotFound
			}
		}
		c.Set(fiber.HeaderContentType, assetContentType(name, data))
		c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
		return c.Send(data)
	}

	app.Get("/", serve)
	app.Get("/static/*", serve)
}

// assetContentType 先按扩展名推断，未知扩展名再按内容嗅探。
func assetContentType(name string, data []byte) string {
	if contentType := mime.TypeByExtension(filepath.Ext(name)); contentType != "" {
		return contentType
	}
	return http.DetectContentType(data)
}
