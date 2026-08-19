package reconciler

import (
	"fmt"

	"github.com/go-logr/logr"
	"github.com/sirupsen/logrus"
)

// logrusSink adapts logr, the interface controller-runtime logs through, onto
// bifrost's logrus logger. Until ctrl.SetLogger is called the framework prints a
// single "log.SetLogger was never called" stack trace and then drops everything
// it has to say — reconciler errors, cache-sync failures, leader-election
// transitions — which is exactly the diagnostic surface an operator needs while
// the reconciler is dark-launched.
//
// It is a hand-written sink rather than logr/funcr because funcr hands the sink
// one pre-rendered string with no severity attached: every framework error would
// arrive as an info line with the message flattened into the text, defeating the
// JSON field output the rest of bifrost logs in.
type logrusSink struct {
	entry *logrus.Entry
}

// NewLogger returns a logr.Logger writing to logger, for ctrl.SetLogger.
func NewLogger(logger *logrus.Logger) logr.Logger {
	return logr.New(logrusSink{entry: logrus.NewEntry(logger)})
}

func (s logrusSink) Init(logr.RuntimeInfo) {}

// Enabled maps logr verbosity onto logrus levels: V(0) is the framework's own
// info output, anything more verbose is per-reconcile chatter that only appears
// when bifrost itself is set to debug.
func (s logrusSink) Enabled(level int) bool {
	if level > 0 {
		return s.entry.Logger.IsLevelEnabled(logrus.DebugLevel)
	}
	return s.entry.Logger.IsLevelEnabled(logrus.InfoLevel)
}

func (s logrusSink) Info(level int, msg string, keysAndValues ...any) {
	entry := s.entry.WithFields(logrusFields(keysAndValues))
	if level > 0 {
		entry.Debug(msg)
		return
	}
	entry.Info(msg)
}

func (s logrusSink) Error(err error, msg string, keysAndValues ...any) {
	s.entry.WithError(err).WithFields(logrusFields(keysAndValues)).Error(msg)
}

func (s logrusSink) WithValues(keysAndValues ...any) logr.LogSink {
	s.entry = s.entry.WithFields(logrusFields(keysAndValues))
	return s
}

// WithName nests the logr name under a single field, so a line's origin
// ("controller-runtime.manager.controller.bifrost-unleash") stays greppable
// without one field per naming level.
func (s logrusSink) WithName(name string) logr.LogSink {
	if existing, ok := s.entry.Data["logger"].(string); ok && existing != "" {
		name = existing + "." + name
	}
	s.entry = s.entry.WithField("logger", name)
	return s
}

// logrusFields converts logr's flat key/value list. logr permits non-string
// keys, which logrus would render unhelpfully, and an odd-length tail, which
// would otherwise index out of range.
func logrusFields(keysAndValues []any) logrus.Fields {
	fields := make(logrus.Fields, len(keysAndValues)/2)
	for i := 0; i+1 < len(keysAndValues); i += 2 {
		key, ok := keysAndValues[i].(string)
		if !ok {
			key = fmt.Sprint(keysAndValues[i])
		}
		fields[key] = keysAndValues[i+1]
	}
	return fields
}
