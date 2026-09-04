/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package flags

import (
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	logsapi "k8s.io/component-base/logs/api/v1"
)

// ConsoleLogFormat is the name of the human-readable, tab-separated log format
// selectable via --logging-format=console:
//
//	2026-09-01T16:02:51.179Z	INFO	webhook/main.go:105	starting webhook server
const ConsoleLogFormat = "console"

func init() {
	// Console is an optional klog format, gated the same way the upstream JSON
	// format is, just at alpha rather than beta.
	if err := logsapi.RegisterLogFormat(ConsoleLogFormat, consoleFactory{}, logsapi.LoggingAlphaOptions); err != nil {
		panic(err)
	}
}

type consoleFactory struct{}

var _ logsapi.LogFormatFactory = consoleFactory{}

func (consoleFactory) Create(c logsapi.LoggingConfiguration, o logsapi.LoggingOptions) (logr.Logger, logsapi.RuntimeControl) {
	encoderConfig := zapcore.EncoderConfig{
		TimeKey:        "ts",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
		EncodeTime: func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
			zapcore.ISO8601TimeEncoder(t.UTC(), enc)
		},
		EncodeLevel: func(l zapcore.Level, enc zapcore.PrimitiveArrayEncoder) {
			// klog V(2) and above map to zap levels below DebugLevel; render
			// them as DEBUG rather than zap's "LEVEL(-2)".
			if l < zapcore.DebugLevel {
				enc.AppendString("DEBUG")
				return
			}
			zapcore.CapitalLevelEncoder(l, enc)
		},
	}

	// Write info messages and errors to stderr to prevent mixing with normal
	// program output, matching the JSON format's non-split-stream behaviour.
	ws := zapcore.Lock(zapcore.AddSync(o.ErrorStream))

	// klog verbosity n corresponds to zap level -n.
	level := zap.NewAtomicLevelAt(zapcore.Level(-c.Verbosity))
	l := zap.New(zapcore.NewCore(zapcore.NewConsoleEncoder(encoderConfig), ws, level), zap.WithCaller(true))

	return zapr.NewLoggerWithOptions(l, zapr.LogInfoLevel(""), zapr.ErrorKey("err")),
		logsapi.RuntimeControl{
			SetVerbosityLevel: func(v uint32) error {
				level.SetLevel(zapcore.Level(-int(v)))
				return nil
			},
			Flush: func() { _ = l.Sync() },
		}
}
