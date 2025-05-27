package utils

import (
	"log"
	"os"
)

// Logger defines a simple interface for logging.
type Logger interface {
	Info(args ...interface{})
	Infof(format string, args ...interface{})
	Error(args ...interface{})
	Errorf(format string, args ...interface{})
	Debug(args ...interface{})
	Debugf(format string, args ...interface{})
}

// simpleLogger is a basic implementation of the Logger interface.
type simpleLogger struct {
	infoLogger  *log.Logger
	errorLogger *log.Logger
	debugLogger *log.Logger
}

// NewSimpleLogger creates a new SimpleLogger.
// It will log info and error messages to stdout and stderr respectively.
// Debug messages are logged to stdout if LOG_LEVEL=DEBUG.
func NewSimpleLogger() Logger {
	return &simpleLogger{
		infoLogger:  log.New(os.Stdout, "INFO: ", log.Ldate|log.Ltime|log.Lshortfile),
		errorLogger: log.New(os.Stderr, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile),
		debugLogger: log.New(os.Stdout, "DEBUG: ", log.Ldate|log.Ltime|log.Lshortfile),
	}
}

func (l *simpleLogger) Info(args ...interface{}) {
	l.infoLogger.Println(args...)
}

func (l *simpleLogger) Infof(format string, args ...interface{}) {
	l.infoLogger.Printf(format, args...)
}

func (l *simpleLogger) Error(args ...interface{}) {
	l.errorLogger.Println(args...)
}

func (l *simpleLogger) Errorf(format string, args ...interface{}) {
	l.errorLogger.Printf(format, args...)
}

func (l *simpleLogger) Debug(args ...interface{}) {
	if os.Getenv("LOG_LEVEL") == "DEBUG" {
		l.debugLogger.Println(args...)
	}
}

func (l *simpleLogger) Debugf(format string, args ...interface{}) {
	if os.Getenv("LOG_LEVEL") == "DEBUG" {
		l.debugLogger.Printf(format, args...)
	}
}

// Global logger instance
var Log Logger

// InitLogger initializes the global logger. This function can be called from main or other setup routines.
// It allows for explicit initialization, which can be better for testing or more complex setups
// than relying solely on a package-level init().
// For this project, the global Log is also initialized by default in an init() func.
func InitLogger(appName, logLevel, logOutput, logFile string) { // Parameters added to match existing InitLogger call in googlechat_test.go
	// For now, we'll just use the simple logger, ignoring the parameters,
	// as the request specifies a simple logger.
	// A more advanced implementation would use these parameters.
	Log = NewSimpleLogger()
}

func init() {
	// Initialize the global logger by default.
	// This can be overridden by an explicit call to InitLogger if needed.
	if Log == nil {
		Log = NewSimpleLogger()
	}
}
