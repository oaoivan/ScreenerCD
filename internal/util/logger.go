package util

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"
)

var (
	// Level controls logging verbosity: "debug", "info", "error"
	Level   = "info"
	Logger  *log.Logger
	logFile *os.File
)

func init() {
	// open or create log file
	var err error
	logDir := "."
	if err = os.MkdirAll(logDir, 0o755); err != nil {
		// fallback to stdout-only logging
		log.Fatalf("failed to create log dir: %v", err)
	}
	logPath := "screner.log"
	logFile, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o666)
	if err != nil {
		log.Fatalf("error opening log file: %v", err)
	}

	mw := io.MultiWriter(os.Stdout, logFile)
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
