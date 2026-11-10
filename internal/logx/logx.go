// Package logx provides leveled logging for the service. Debug logs are only
// emitted when verbose mode is enabled (SVMA_VERBOSE=1 or LOG_LEVEL=debug).
package logx

import "log"

var verbose bool

// SetVerbose enables or disables debug logging.
func SetVerbose(value bool) { verbose = value }

// Verbose reports whether debug logging is enabled.
func Verbose() bool { return verbose }

// Debugf logs only in verbose mode.
func Debugf(format string, args ...any) {
	if verbose {
		log.Printf(format, args...)
	}
}

// Infof always logs.
func Infof(format string, args ...any) {
	log.Printf(format, args...)
}
