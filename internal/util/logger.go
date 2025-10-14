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

const (
	maxLogLines   = 1000
	logBufferSize = 64 * 1024
)

var (
	// Level controls logging verbosity: "debug", "info", "error"
	Level     = "info"
	Logger    *log.Logger
	logWriter *lineLimitedWriter
)

// lineLimitedWriter keeps only the freshest maxLines lines in the log file.
type lineLimitedWriter struct {
	file     *os.File
	maxLines int
	mu       sync.Mutex
	buffer   []byte
	lines    []int64
}

func newLineLimitedWriter(path string, maxLines int) (*lineLimitedWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		return nil, err
	}
	llw := &lineLimitedWriter{
		file:     f,
		maxLines: maxLines,
		buffer:   make([]byte, logBufferSize),
	}
	if err := llw.rebuildOffsets(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return llw, nil
}

func (w *lineLimitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	end, err := w.file.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}

	n, err := w.file.Write(p)
	if err != nil {
		return n, err
	}
	w.appendOffsetsLocked(p[:n], end)
	if err := w.ensureLimitLocked(); err != nil {
		return n, err
	}
	if err := w.file.Sync(); err != nil {
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

func (w *lineLimitedWriter) rebuildOffsets() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.lines = w.lines[:0]
	info, err := w.file.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size == 0 {
		return nil
	}

	buf := w.buffer
	if len(buf) == 0 {
		buf = make([]byte, logBufferSize)
		w.buffer = buf
	}

	var (
		pos int64
	)
	for pos < size {
		chunk := int64(len(buf))
		remaining := size - pos
		if chunk > remaining {
			chunk = remaining
		}
		intChunk := int(chunk)
		if intChunk == 0 {
			break
		}
		data := buf[:intChunk]
		n, err := w.file.ReadAt(data, pos)
		if err != nil && err != io.EOF {
			return err
		}
		for i := 0; i < n; i++ {
			if data[i] == '\n' {
				w.lines = append(w.lines, pos+int64(i))
			}
		}
		pos += int64(n)
		if err == io.EOF {
			break
		}
	}
	return w.ensureLimitLocked()
}

func (w *lineLimitedWriter) appendOffsetsLocked(p []byte, base int64) {
	for i, b := range p {
		if b == '\n' {
			w.lines = append(w.lines, base+int64(i))
		}
	}
}

func (w *lineLimitedWriter) ensureLimitLocked() error {
	if len(w.lines) <= w.maxLines {
		return nil
	}
	excess := len(w.lines) - w.maxLines
	cutoff := w.lines[excess-1] + 1
	if err := w.compactLocked(cutoff); err != nil {
		return err
	}
	for i := excess; i < len(w.lines); i++ {
		w.lines[i-excess] = w.lines[i] - cutoff
	}
	w.lines = w.lines[:len(w.lines)-excess]
	return nil
}

func (w *lineLimitedWriter) compactLocked(cutoff int64) error {
	info, err := w.file.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if cutoff <= 0 {
		return nil
	}
	if cutoff >= size {
		if err := w.file.Truncate(0); err != nil {
			return err
		}
		_, err = w.file.Seek(0, io.SeekEnd)
		return err
	}

	buf := w.buffer
	if len(buf) == 0 {
		buf = make([]byte, logBufferSize)
		w.buffer = buf
	}

	readOffset := cutoff
	writeOffset := int64(0)
	for readOffset < size {
		chunk := int64(len(buf))
		remaining := size - readOffset
		if chunk > remaining {
			chunk = remaining
		}
		intChunk := int(chunk)
		if intChunk == 0 {
			break
		}
		data := buf[:intChunk]
		n, err := w.file.ReadAt(data, readOffset)
		if err != nil && err != io.EOF {
			return err
		}
		if n <= 0 {
			break
		}
		if _, err := w.file.WriteAt(data[:n], writeOffset); err != nil {
			return err
		}
		readOffset += int64(n)
		writeOffset += int64(n)
		if err == io.EOF {
			break
		}
	}

	if err := w.file.Truncate(size - cutoff); err != nil {
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
	logWriter, err = newLineLimitedWriter(logPath, maxLogLines)
	if err != nil {
		log.Fatalf("error opening log file: %v", err)
	}

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
	writers = append(writers, logWriter)
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
	if logWriter != nil {
		_ = logWriter.Sync()
		_ = logWriter.Close()
	}
	os.Exit(1)
}
