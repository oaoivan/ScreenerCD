package util

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const maxLogLines = 1000

var (
	// Level controls logging verbosity: "debug", "info", "error"
	Level   = "info"
	Logger  *log.Logger
	logFile *os.File
)

type lineLimitedWriter struct {
	file     *os.File
	maxLines int
	mu       sync.Mutex
}

func newLineLimitedWriter(path string, maxLines int) (*lineLimitedWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o666)
	if err != nil {
		return nil, err
	}
	llw := &lineLimitedWriter{file: f, maxLines: maxLines}
	if err := llw.trimUnlocked(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return llw, nil
}

func (w *lineLimitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.file.Write(p)
	if err != nil {
		return n, err
	}
	if err := w.trimUnlocked(); err != nil {
		return n, err
	}
	return n, nil
}

func (w *lineLimitedWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

func (w *lineLimitedWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Sync()
}

func (w *lineLimitedWriter) trimUnlocked() error {
	info, err := w.file.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size <= 0 {
		return nil
	}

	var (
		offset             = size
		newlineCount       = 0
		cutoff       int64 = 0
	)
	const bufSize = 8192
	buf := make([]byte, bufSize)

	for offset > 0 && newlineCount <= w.maxLines {
		readSize := bufSize
		if int64(readSize) > offset {
			readSize = int(offset)
		}
		offset -= int64(readSize)
		if _, err := w.file.ReadAt(buf[:readSize], offset); err != nil {
			return err
		}
		for i := readSize - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				newlineCount++
				if newlineCount > w.maxLines {
					cutoff = offset + int64(i) + 1
					break
				}
			}
		}
		if cutoff != 0 {
			break
		}
	}

	if newlineCount <= w.maxLines || cutoff == 0 {
		return nil
	}

	tailLen := size - cutoff
	tail := make([]byte, tailLen)
	if _, err := w.file.ReadAt(tail, cutoff); err != nil {
		return err
	}

	if err := w.file.Truncate(0); err != nil {
		return err
	}
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := w.file.Write(tail); err != nil {
		return err
	}
	if err := w.file.Sync(); err != nil {
		return err
	}
	_, err = w.file.Seek(0, io.SeekEnd)
	return err
}

func init() {
	// open or create log file
	var err error
	logDir := "."
	if err = os.MkdirAll(logDir, 0o755); err != nil {
		// fallback to stdout-only logging
		log.Fatalf("failed to create log dir: %v", err)
	}
	logPath := "screner.log"
	limitedWriter, err := newLineLimitedWriter(logPath, maxLogLines)
	if err != nil {
		log.Fatalf("error opening log file: %v", err)
	}
	logFile = limitedWriter.file

	logToStdout := true
	if env := strings.TrimSpace(os.Getenv("SCR_LOG_STDOUT")); env != "" {
		switch strings.ToLower(env) {
		case "0", "false", "no", "off":
			logToStdout = false
		}
	}
	writers := make([]io.Writer, 0, 2)
	if logToStdout {
		writers = append(writers, os.Stdout)
	}
	writers = append(writers, limitedWriter)
	mw := io.MultiWriter(writers...)
	Logger = log.New(mw, "", log.Ldate|log.Ltime|log.Lshortfile)
	Infof("logger initialized, output=%s", logPath)
}

// SetLevel sets the logging level. Valid: debug, info, error
func SetLevel(l string) {
	Level = strings.ToLower(l)
	Infof("log level set to %s", Level)
}

// internal helper to format time-prefixed messages
func format(formatStr string, v ...interface{}) string {
	ts := time.Now().Format(time.RFC3339)
	msg := fmt.Sprintf(formatStr, v...)
	return fmt.Sprintf("%s %s", ts, msg)
}

// Debugf logs debug messages when level==debug
func Debugf(formatStr string, v ...interface{}) {
	if Level != "debug" {
		return
	}
	Logger.Output(2, format("[DEBUG] "+formatStr, v...))
}

// Infof logs informational messages
func Infof(formatStr string, v ...interface{}) {
	Logger.Output(2, format("[INFO] "+formatStr, v...))
}

// Errorf logs error messages
func Errorf(formatStr string, v ...interface{}) {
	Logger.Output(2, format("[ERROR] "+formatStr, v...))
}

// Fatalf logs fatal messages and exits the application
func Fatalf(formatStr string, v ...interface{}) {
	Logger.Output(2, format("[FATAL] "+formatStr, v...))
	if logFile != nil {
		_ = logFile.Sync()
		_ = logFile.Close()
	}
	os.Exit(1)
}
