package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
)

func main() {
	port, dir := parseArgs(os.Args[1:])

	// root 必须提前解析成规范绝对路径，后续所有越权判断都以它为基准
	root, err := filepath.Abs(dir)
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		logf("ERROR 无法访问目录 %q: %v", dir, err)
		os.Exit(1)
	}

	logf("INFO serving %s on http://0.0.0.0:%d", root, port)
	err = newApp(root, port).Listen(
		fmt.Sprintf(":%d", port),
		fiber.ListenConfig{DisableStartupMessage: true},
	)
	if err != nil {
		logf("ERROR %v", err)
		os.Exit(1)
	}
}

// parseArgs 从命令行参数解析端口与目录：能解析为 u16 的当作端口，其余当作目录，同类参数后者覆盖前者。
func parseArgs(args []string) (uint16, string) {
	port := uint16(8000)
	dir := "."
	for _, arg := range args {
		if parsed, err := strconv.ParseUint(arg, 10, 16); err == nil {
			port = uint16(parsed)
		} else {
			dir = arg
		}
	}
	return port, dir
}

// logf 按 "2026-01-01 12:00:00 LEVEL msg" 的格式往 stdout 写一行日志。
func logf(format string, args ...any) {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	fmt.Fprintf(os.Stdout, "%s %s\n", timestamp, fmt.Sprintf(format, args...))
}
