package main

import (
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
)

func registerFiles(app *fiber.App, root string) {
	app.Get("/files/*", func(c fiber.Ctx) error {
		abs, ok := resolveFile(root, wildcard(c))
		if !ok {
			return fiber.ErrNotFound
		}
		return sendFile(c, abs)
	})
}

// resolveFile 解析请求路径对应的绝对文件路径，且必须位于 root 之内（防目录穿越）。
//
// 先用 Lstat 判断类型：符号链接不会被当作文件服务，
// 避免 EvalSymlinks 跟随符号链接逃逸 root 或引入 TOCTOU 窗口。
func resolveFile(root, sub string) (string, bool) {
	joined := filepath.Join(root, filepath.FromSlash(sub))
	info, err := os.Lstat(joined)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	canonical, err := filepath.EvalSymlinks(joined)
	if err != nil || !withinRoot(root, canonical) {
		return "", false
	}
	return canonical, true
}

// sendFile 用 sendfile(2) 把文件送进连接。
//
// fasthttp 对已知长度的普通 TCP 响应走 writeBodyFixedSize → copyZeroAlloc →
// net.TCPConn.ReadFrom，内核零拷贝把页缓存直接写进网卡，文件内容不经过用户态。
// ByteRange 打开后同时获得 Accept-Ranges/206 与 If-Modified-Since/304。
func sendFile(c fiber.Ctx, abs string) error {
	contentType := mime.TypeByExtension(filepath.Ext(abs))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	name := filepath.Base(abs)

	c.Set(fiber.HeaderContentType, contentType)
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	c.Set(fiber.HeaderContentDisposition, contentDisposition(name, contentType))

	if etag, ok := fileETag(abs); ok {
		c.Set(fiber.HeaderETag, etag)
		if matchesIfNoneMatch(c.Get(fiber.HeaderIfNoneMatch), etag) {
			return c.SendStatus(fiber.StatusNotModified)
		}
	}

	// Fiber 把路径当 URI 交给 fasthttp 的文件处理器，文件名里的 # ? 会被当成
	// fragment/query 分隔符而找不到文件，所以先转义一次，由 fasthttp 解码回真实路径。
	return c.SendFile((&url.URL{Path: abs}).EscapedPath(), fiber.SendFile{ByteRange: true})
}

// fileETag 按 Apache 风格生成文件校验值；取不到元信息时返回 false，此时不发 ETag。
func fileETag(abs string) (string, bool) {
	info, err := os.Stat(abs)
	if err != nil {
		return "", false
	}
	return `"` + strings.Join(
		[]string{
			strconv.FormatInt(info.Size(), 16),
			strconv.FormatInt(info.ModTime().Unix(), 16),
			strconv.FormatInt(int64(info.ModTime().Nanosecond()), 16),
		}, "-",
	) + `"`, true
}

// matchesIfNoneMatch 判断 If-None-Match 是否命中当前 ETag（支持 "*"、弱校验前缀与逗号分隔列表）。
func matchesIfNoneMatch(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(candidate), "W/")) == etag {
			return true
		}
	}
	return false
}

// contentDisposition 与 salvo::NamedFile 保持一致：文本、图片、音视频与 JS/JSON 内联展示，
// XML 类文档（可能被浏览器当作文档执行脚本）及其余类型一律作为附件下载。
func contentDisposition(name, contentType string) string {
	mediaType, _, _ := strings.Cut(contentType, ";")
	top, subtype, ok := strings.Cut(strings.TrimSpace(mediaType), "/")
	inline := ok && !isScriptableXML(subtype) &&
		(top == "text" || top == "image" || top == "video" || top == "audio" ||
			subtype == "javascript" || subtype == "json")
	if inline {
		return "inline"
	}

	escaped := escapeQuotedFilename(name)
	disposition := `attachment; filename="` + escaped + `"`
	if escaped != name {
		disposition += "; filename*=UTF-8''" + percentEncode(name, rfc5987AttrChar)
	}
	return disposition
}

// isScriptableXML 判断 MIME 子类型是否属于会被浏览器当作 XML 文档解析的类型。
func isScriptableXML(subtype string) bool {
	if strings.HasSuffix(subtype, "+xml") {
		return true
	}
	head, _, _ := strings.Cut(subtype, "-")
	return strings.EqualFold(head, "xml") || strings.EqualFold(subtype, "xsl")
}

// escapeQuotedFilename 转义 filename= 引号形式里不允许直接出现的字符。
func escapeQuotedFilename(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r > 0x7e:
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// rfc5987AttrChar 报告字节 b 能否在 RFC 5987 的 attr-char 中原样出现。
func rfc5987AttrChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
		strings.IndexByte("!#$&+-.^_`|~", b) >= 0
}

// percentEncode 对不满足 keep 的字节做 %XX 编码。
func percentEncode(s string, keep func(byte) bool) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if keep(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}
