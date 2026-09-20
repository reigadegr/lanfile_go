package main

import (
	"bufio"
	"fmt"
	"os"
	"time"
)

// logQueueSize 是日志队列长度；队列满时丢弃新日志。
const logQueueSize = 8192

var logQueue = make(chan string, logQueueSize)

func init() {
	go writeLogs()
}

// logf 按 "2026-01-01 12:00:00 LEVEL msg" 的格式投递一行日志。
//
// 请求路径只负责入队，真正写 stdout 由后台 goroutine 批量完成。
// 这样慢终端（或没人读的管道）最多让日志被丢掉，不会把响应处理一起堵死——
// 原 Rust 版用 tracing_appender::non_blocking 就是同样的设计。
func logf(format string, args ...any) {
	select {
	case logQueue <- logLine(format, args...):
	default: // 队列已满：丢弃该行，绝不在请求路径上阻塞
	}
}

// logfSync 立即同步写一行日志，用于启动与退出这类只出现一次的关键信息，
// 避免 os.Exit 抢在后台 goroutine 之前把消息丢掉。
func logfSync(format string, args ...any) {
	_, _ = os.Stdout.WriteString(logLine(format, args...))
}

// logLine 拼出完整的一行日志（含换行）。
func logLine(format string, args ...any) string {
	return time.Now().Format("2006-01-02 15:04:05") + " " + fmt.Sprintf(format, args...) + "\n"
}

// writeLogs 批量消费日志队列：把当下能取到的行攒成一次写，减少慢终端的系统调用次数。
func writeLogs() {
	out := bufio.NewWriterSize(os.Stdout, 64*1024)
	for line := range logQueue {
		_, _ = out.WriteString(line)
		for len(logQueue) > 0 {
			_, _ = out.WriteString(<-logQueue)
		}
		_ = out.Flush()
	}
}
