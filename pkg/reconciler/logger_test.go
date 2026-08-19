package reconciler

import (
	"bytes"
	"encoding/json"
	"errors"
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
