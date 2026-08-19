package server

import (
	"testing"

	"github.com/nais/bifrost/pkg/config"
	"github.com/sirupsen/logrus"
)

// The chart has exposed backend.logLevel since before the reconciler existed
// while the logger was hardcoded to debug, which also switched on every
// controller-runtime verbosity level in production.
func TestInitLogger_HonoursTheConfiguredLevel(t *testing.T) {
	cases := []struct {
		configured string
		want       logrus.Level
	}{
		{"info", logrus.InfoLevel},
		{"warn", logrus.WarnLevel},
		{"debug", logrus.DebugLevel},
		{"trace", logrus.TraceLevel},
		{"", logrus.InfoLevel},            // unset is the default, not a misconfiguration
		{"not-a-level", logrus.InfoLevel}, // a typo must not take the process down
	}

	for _, tc := range cases {
		t.Run(tc.configured, func(t *testing.T) {
			logger := initLogger(&config.Config{LogLevel: tc.configured})
			if got := logger.GetLevel(); got != tc.want {
				t.Errorf("log level = %s, want %s", got, tc.want)
			}
		})
	}
}
