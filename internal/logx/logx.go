// Package logx builds the rotating application logger.
package logx

import (
	"io"
	"log"
	"os"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"
)

// maxBackups returns how many rotated files fit in totalMB beside the live one.
func maxBackups(fileMB, totalMB int) int {
	if fileMB <= 0 {
		return 0
	}
	return max(totalMB/fileMB-1, 0)
}

// New returns a logger writing to a size-rotated file, and also to stdout if asked.
func New(path string, fileMB, totalMB int, alsoStdout bool) *log.Logger {
	var w io.Writer = &lumberjack.Logger{
		Filename:   path,
		MaxSize:    fileMB,
		MaxBackups: maxBackups(fileMB, totalMB),
		Compress:   false,
	}
	if alsoStdout {
		w = io.MultiWriter(os.Stdout, w)
	}
	return log.New(w, "", log.LstdFlags)
}

// Redact replaces every non-empty secret in s with "***".
func Redact(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}
