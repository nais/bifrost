package reconciler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/sirupsen/logrus"
)

// captureLogger returns a JSON logger writing into buf, matching how bifrost
// logs in production so the sink's field mapping is exercised end to end.
func captureLogger(level logrus.Level) (*logrus.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	logger := logrus.New()
	logger.SetOutput(buf)
	logger.SetFormatter(&logrus.JSONFormatter{})
	logger.SetLevel(level)
	return logger, buf
}

func TestLogrusSink_MapsLogrOutputToLogrus(t *testing.T) {
	cases := []struct {
		name  string
		level logrus.Level
		log   func(*testing.T, *logrus.Logger)
		want  map[string]string
		empty bool
	}{
		{
			name:  "error keeps its severity and error value",
			level: logrus.InfoLevel,
			log: func(_ *testing.T, l *logrus.Logger) {
				NewLogger(l).WithName("controller").Error(errors.New("boom"), "Reconciler error", "instance", "team-a")
			},
			want: map[string]string{
				"level":    "error",
				"msg":      "Reconciler error",
				"error":    "boom",
				"logger":   "controller",
				"instance": "team-a",
			},
		},
		{
			name:  "nested names are joined",
			level: logrus.InfoLevel,
			log: func(_ *testing.T, l *logrus.Logger) {
				NewLogger(l).WithName("manager").WithName("bifrost-unleash").Info("Starting workers")
			},
			want: map[string]string{
				"level":  "info",
				"msg":    "Starting workers",
				"logger": "manager.bifrost-unleash",
			},
		},
		{
			name:  "values carried on the logger are kept",
			level: logrus.InfoLevel,
			log: func(_ *testing.T, l *logrus.Logger) {
				NewLogger(l).WithValues("controller", "bifrost-unleash").Info("Starting")
			},
			want: map[string]string{"controller": "bifrost-unleash"},
		},
		{
			name:  "verbose lines are debug, and dropped at info",
			level: logrus.InfoLevel,
			log: func(_ *testing.T, l *logrus.Logger) {
				NewLogger(l).V(1).Info("chatter")
			},
			empty: true,
		},
		{
			name:  "verbose lines appear when debug is on",
			level: logrus.DebugLevel,
			log: func(_ *testing.T, l *logrus.Logger) {
				NewLogger(l).V(1).Info("chatter")
			},
			want: map[string]string{"level": "debug", "msg": "chatter"},
		},
		{
			name:  "per-item framework chatter needs trace, not debug",
			level: logrus.DebugLevel,
			log: func(_ *testing.T, l *logrus.Logger) {
				NewLogger(l).V(5).Info("Reconciling")
			},
			empty: true,
		},
		{
			name:  "per-item framework chatter appears at trace",
			level: logrus.TraceLevel,
			log: func(_ *testing.T, l *logrus.Logger) {
				NewLogger(l).V(5).Info("Reconciling")
			},
			want: map[string]string{"level": "trace", "msg": "Reconciling"},
		},
		{
			name:  "a keysAndValues pair named error does not shadow the real one",
			level: logrus.InfoLevel,
			log: func(_ *testing.T, l *logrus.Logger) {
				NewLogger(l).Error(errors.New("the real failure"), "Reconciler error", "error", "a caller-supplied string")
			},
			want: map[string]string{"error": "the real failure"},
		},
		{
			name:  "a dangling key does not panic",
			level: logrus.InfoLevel,
			log: func(_ *testing.T, l *logrus.Logger) {
				NewLogger(l).Info("odd", "paired", "value", "dangling")
			},
			want: map[string]string{"msg": "odd", "paired": "value"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := captureLogger(tc.level)
			tc.log(t, logger)

			if tc.empty {
				if buf.Len() != 0 {
					t.Fatalf("expected no output, got %s", buf.String())
				}
				return
			}

			line := map[string]any{}
			if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
				t.Fatalf("unmarshal %q: %v", buf.String(), err)
			}
			for k, want := range tc.want {
				if got, _ := line[k].(string); got != want {
					t.Errorf("field %q = %q, want %q (line: %s)", k, got, want, buf.String())
				}
			}
		})
	}
}

// Enabled is what controller-runtime asks before it renders a line at all, so a
// wrong answer here is the difference between silence and one log line per
// instance per resync. It is checked directly because the table above can only
// observe it through output that has already been formatted.
func TestLogrusSink_EnabledFollowsTheConfiguredLevel(t *testing.T) {
	cases := []struct {
		level     logrus.Level
		verbosity int
		want      bool
	}{
		{logrus.WarnLevel, 0, false}, // the framework's own info output is below warn
		{logrus.InfoLevel, 0, true},
		{logrus.InfoLevel, 1, false},
		{logrus.DebugLevel, 1, true},
		{logrus.DebugLevel, 3, true},
		{logrus.DebugLevel, 4, false}, // per-item chatter must be asked for
		{logrus.DebugLevel, 5, false},
		{logrus.TraceLevel, 5, true},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/V(%d)", tc.level, tc.verbosity), func(t *testing.T) {
			logger, _ := captureLogger(tc.level)
			if got := NewLogger(logger).V(tc.verbosity).Enabled(); got != tc.want {
				t.Errorf("V(%d).Enabled() at %s = %v, want %v", tc.verbosity, tc.level, got, tc.want)
			}
		})
	}
}

// ctrl.SetLogger runs before anything else, and controller-runtime calls
// Enabled on the sink immediately: a nil logger used to panic there rather than
// at the call site that forgot to pass one.
func TestNewLogger_NilLoggerDoesNotPanic(t *testing.T) {
	NewLogger(nil).V(1).Info("no logger configured")
}
