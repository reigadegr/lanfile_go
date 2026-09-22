package main

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
)

// 打开文件缓存：按请求路径缓存已经打开的 *os.File 与该描述符的元信息。
//
// 命中时只需一次 Lstat 重新校验 (inode, size, mtime)，就能省掉 open/close、
// filepath.EvalSymlinks 的逐级 Lstat、mime 推断以及 fasthttp 自带的文件缓存查表。
//
// 16 个分片 × 32 条 = 最多 512 条；分片内按「分片时钟 + 条目 tick」做 LRU 淘汰。
const (
	cacheShards    = 16
	cacheShardSize = 32
)

// maxCachedFileSize 与 fasthttp 的 maxSmallFileSize 一致：超过它的文件不缓存。
//
// 原因是共享 fd 没法安全地 sendfile：Linux 上 sendfile(2) 的 offset 传 NULL 时
// 用的是「打开文件描述」里的文件偏移量，多个请求共用一个 fd 会互相推进偏移量，
// 读出来的正文会互相截断；dup 出来的 fd 共享同一个偏移量，同样不行。
// fasthttp 自己对大文件也是每次请求重新 open 一个新 fd 才用 sendfile 的，
// 所以这里把大文件原样交给框架，避免丢掉零拷贝路径。
const maxCachedFileSize = 2 * 4096

// errNotCacheable 表示文件打开了但不适合进缓存（不是普通文件或体积过大）。
var errNotCacheable = errors.New("file not cacheable")

// cacheEntry 是缓存中的一条记录：已打开的 fd、该描述符的元信息，以及预先算好的响应头。
type cacheEntry struct {
	key    string // 请求路径（分片内唯一）
	joined string // root 拼接请求路径，每次命中都用它重新 Lstat

	file *os.File
	info os.FileInfo // 插入时 fstat 的结果，用 os.SameFile 比对 inode

	size  int64
	mtime time.Time

	contentType  string
	disposition  string
	etag         string
	lastModified string

	tick  uint64 // 最后一次使用时的分片时钟值，LRU 依据
	refs  int    // 正在使用该条目的响应数
	dying bool   // 已从缓存摘除，等 refs 归零后再关 fd
}

// cacheShard 是缓存的一个分片，每个分片各自加锁，避免所有请求争同一把锁。
type cacheShard struct {
	mu      sync.Mutex
	clock   uint64
	entries map[string]*cacheEntry
	list    []*cacheEntry
}

// fileCache 按请求路径缓存已打开的文件描述符。
type fileCache struct {
	shards [cacheShards]cacheShard
	dirs   dirCache

	rootDir *os.File // root 目录句柄，openat2 的 dirfd；保持引用以免被 GC 回收
	rootFd  int      // rootDir 的描述符号；不支持 openat2 时为 -1
}

func newFileCache(root string) *fileCache {
	cache := &fileCache{}
	for i := range cache.shards {
		cache.shards[i].entries = make(map[string]*cacheEntry, cacheShardSize)
	}
	cache.dirs.entries = make(map[string]os.FileInfo, dirCacheSize)
	cache.rootDir, cache.rootFd = openRoot(root)
	return cache
}

// dirCacheSize 是目录解析缓存的容量上限；目录数量通常远小于文件数量。
const dirCacheSize = 256

// dirCache 缓存「某个目录路径本身不含符号链接，且位于 root 之内」这一结论。
//
// 命中时只需一次 Stat 比对目录 inode，就能省掉 filepath.EvalSymlinks 对每一级路径的
// Lstat 与字符串处理；含符号链接的路径不缓存，仍走完整解析，语义与改动前一致。
type dirCache struct {
	mu      sync.Mutex
	entries map[string]os.FileInfo
}

// resolve 返回 joined 的父目录本身与它的规范路径。父目录不在 root 之内时返回 false。
//
// 缓存只记录「父目录解析后仍是它自己」的情况：此时 Stat 与插入时的目录 inode 一致，
// 就说明该路径仍然指向当初校验过、且位于 root 之内的那个目录。
func (dc *dirCache) resolve(root, joined string) (dir, canonical string, ok bool) {
	dir = filepath.Dir(joined)
	info, err := os.Stat(dir)
	if err != nil {
		return "", "", false
	}

	dc.mu.Lock()
	cached, hit := dc.entries[dir]
	dc.mu.Unlock()
	if hit && os.SameFile(cached, info) {
		return dir, dir, true
	}

	canonical, err = filepath.EvalSymlinks(dir)
	if err != nil || !withinRoot(root, canonical) {
		return "", "", false
	}
	if canonical == dir {
		dc.store(dir, info)
	}
	return dir, canonical, true
}

func (dc *dirCache) store(dir string, info os.FileInfo) {
	dc.mu.Lock()
	if len(dc.entries) >= dirCacheSize {
		for key := range dc.entries { // 目录数量很少，满了随便丢一条即可
			delete(dc.entries, key)
			break
		}
	}
	dc.entries[dir] = info
	dc.mu.Unlock()
}

// shardOf 用 FNV-1a 把请求路径映射到分片。路径完全来自请求，散列必须足够散。
func (fc *fileCache) shardOf(key string) *cacheShard {
	hash := uint32(2166136261)
	for i := 0; i < len(key); i++ {
		hash = (hash ^ uint32(key[i])) * 16777619
	}
	return &fc.shards[hash&(cacheShards-1)]
}

// nextTick 推进分片时钟；调用方必须已持有分片锁。
func (s *cacheShard) nextTick() uint64 {
	s.clock++
	return s.clock
}

// acquire 取出条目并增加引用计数；没有缓存时返回 nil。
// 每个非 nil 的返回值都必须归还：命中后由正文读取器在 fasthttp 调用 Close 时归还，
// 校验失败则由 discard 归还。
func (fc *fileCache) acquire(key string) *cacheEntry {
	shard := fc.shardOf(key)
	shard.mu.Lock()
	entry := shard.entries[key]
	if entry != nil {
		entry.refs++
		entry.tick = shard.nextTick()
	}
	shard.mu.Unlock()
	return entry
}

// release 归还一个引用；条目已被摘除且引用归零时关闭 fd。
func (fc *fileCache) release(entry *cacheEntry) {
	shard := fc.shardOf(entry.key)
	shard.mu.Lock()
	entry.refs--
	if entry.refs == 0 && entry.dying {
		_ = entry.file.Close()
		entry.file = nil
	}
	shard.mu.Unlock()
}

// discard 把不再匹配（或已不存在）的条目摘出缓存，并归还调用方的引用。
func (fc *fileCache) discard(entry *cacheEntry) {
	shard := fc.shardOf(entry.key)
	shard.mu.Lock()
	fc.unlink(shard, entry)
	shard.mu.Unlock()
	fc.release(entry)
}

// unlink 从分片里摘除条目；仍有响应在用它时只标记 dying，等最后一个引用归还再关 fd。
// 调用方必须已持有分片锁。
func (fc *fileCache) unlink(shard *cacheShard, entry *cacheEntry) {
	entry.dying = true
	if shard.entries[entry.key] != entry {
		return
	}
	delete(shard.entries, entry.key)
	for i, candidate := range shard.list {
		if candidate == entry {
			last := len(shard.list) - 1
			shard.list[i] = shard.list[last]
			shard.list[last] = nil
			shard.list = shard.list[:last]
			break
		}
	}
}

// insert 把条目放进分片，必要时按 LRU 淘汰最久未使用的条目。
func (fc *fileCache) insert(entry *cacheEntry) {
	shard := fc.shardOf(entry.key)
	shard.mu.Lock()
	if old := shard.entries[entry.key]; old != nil {
		// 同一路径被并发打开：保留新条目，旧条目等最后一个引用归还再关闭。
		fc.unlink(shard, old)
		if old.refs == 0 {
			_ = old.file.Close()
			old.file = nil
		}
	}
	for len(shard.list) >= cacheShardSize {
		victim := shard.list[0]
		for _, candidate := range shard.list[1:] {
			if candidate.tick < victim.tick {
				victim = candidate
			}
		}
		fc.unlink(shard, victim)
		if victim.refs == 0 {
			_ = victim.file.Close()
			victim.file = nil
		}
	}
	entry.tick = shard.nextTick()
	shard.entries[entry.key] = entry
	shard.list = append(shard.list, entry)
	shard.mu.Unlock()
}

// store 打开文件、读取描述符元信息并写入缓存，返回一个已持有引用的条目。
func (fc *fileCache) store(key, joined, abs string) (*cacheEntry, error) {
	file, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxCachedFileSize {
		_ = file.Close()
		return nil, errNotCacheable
	}
	return fc.storeOpened(key, joined, file, info), nil
}

// storeOpened 用已经打开的 fd 建立缓存条目，返回一个已持有引用的条目。
func (fc *fileCache) storeOpened(key, joined string, file *os.File, info os.FileInfo) *cacheEntry {
	contentType := mime.TypeByExtension(filepath.Ext(joined))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	entry := &cacheEntry{
		key:          strings.Clone(key),
		joined:       joined,
		file:         file,
		info:         info,
		size:         info.Size(),
		mtime:        info.ModTime(),
		contentType:  contentType,
		disposition:  contentDisposition(filepath.Base(joined), contentType),
		etag:         etagOf(info),
		lastModified: info.ModTime().UTC().Format(http.TimeFormat),
		refs:         1,
	}
	fc.insert(entry)
	return entry
}

// matches 用请求路径的 Lstat 结果校验缓存条目：
// inode（含设备号）、大小、mtime（含纳秒）必须全部一致，否则视为失效。
func (entry *cacheEntry) matches(info os.FileInfo) bool {
	return info.Size() == entry.size &&
		info.ModTime().Equal(entry.mtime) &&
		os.SameFile(entry.info, info)
}

// serve 直接用缓存条目构造响应：状态、Content-Type、Content-Length、Last-Modified、
// Accept-Ranges 全部自己写，正文交给 fasthttp 从缓存 fd 读，不再 stat / open / close。
func (fc *fileCache) serve(c fiber.Ctx, entry *cacheEntry) error {
	c.Set(fiber.HeaderContentType, entry.contentType)
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	c.Set(fiber.HeaderContentDisposition, entry.disposition)
	c.Set(fiber.HeaderETag, entry.etag)

	if matchesIfNoneMatch(c.Get(fiber.HeaderIfNoneMatch), entry.etag) {
		fc.release(entry)
		return c.SendStatus(fiber.StatusNotModified)
	}
	// 与 fasthttp 的 ctx.IfModifiedSince + ctx.NotModified 保持一致：
	// 命中 If-Modified-Since 时整个响应被重置，只留 304 状态码。
	if !ifModifiedSince(c.Get(fiber.HeaderIfModifiedSince), entry.mtime) {
		fc.release(entry)
		c.Response().Reset()
		c.Response().SetStatusCode(fiber.StatusNotModified)
		return nil
	}

	c.Set(fiber.HeaderAcceptRanges, "bytes")
	c.Set(fiber.HeaderLastModified, entry.lastModified)

	if c.Method() == fiber.MethodHead {
		// 与 fasthttp 的文件处理器一致：HEAD 只回头，不读正文。
		fc.release(entry)
		c.Response().ResetBody()
		c.Response().SkipBody = true
		c.Response().Header.SetContentLength(int(entry.size))
		return nil
	}

	c.Response().SetBodyStream(&fileBody{cache: fc, entry: entry, size: entry.size}, int(entry.size))
	return nil
}

// ifModifiedSince 复刻 fasthttp 的 ctx.IfModifiedSince：请求头缺失或解析失败时都返回 true。
func ifModifiedSince(header string, mtime time.Time) bool {
	if header == "" {
		return true
	}
	since, err := time.Parse(http.TimeFormat, header)
	if err != nil {
		return true
	}
	return since.Before(mtime.Truncate(time.Second))
}

// fileBody 是响应正文读取器。它用 ReadAt 读缓存 fd，因此多个响应可以共享同一个 fd
// 而互不影响文件偏移量；Close 由 fasthttp 在响应写完后调用，用来归还缓存条目的引用。
type fileBody struct {
	cache *fileCache
	entry *cacheEntry
	off   int64
	size  int64
}

func (b *fileBody) Read(p []byte) (int, error) {
	if b.off >= b.size {
		return 0, io.EOF
	}
	if int64(len(p)) > b.size-b.off {
		p = p[:b.size-b.off]
	}
	n, err := b.entry.file.ReadAt(p, b.off)
	b.off += int64(n)
	return n, err
}

func (b *fileBody) Close() error {
	b.cache.release(b.entry)
	b.entry = nil
	return nil
}

// etagOf 生成与改动前 fileETag 完全相同的校验值：size、mtime 秒、mtime 纳秒的十六进制。
func etagOf(info os.FileInfo) string {
	return `"` + strings.Join(
		[]string{
			strconv.FormatInt(info.Size(), 16),
			strconv.FormatInt(info.ModTime().Unix(), 16),
			strconv.FormatInt(int64(info.ModTime().Nanosecond()), 16),
		}, "-",
	) + `"`
}
