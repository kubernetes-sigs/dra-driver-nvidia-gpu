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
	"bytes"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	logsapi "k8s.io/component-base/logs/api/v1"
)

// TestConsoleFormat checks that the console format renders klog messages as
// timestamp, level, caller and message, and that V() levels are gated by the
// configured verbosity.
func TestConsoleFormat(t *testing.T) {
	var buf bytes.Buffer
	c := logsapi.NewLoggingConfiguration()
	c.Format = ConsoleLogFormat
	c.Verbosity = 4

	logger, control := consoleFactory{}.Create(*c, logsapi.LoggingOptions{ErrorStream: &buf})

	logger.Info("starting webhook server")
	logger.V(2).Info("handling request")
	logger.V(6).Info("must not appear")
	logger.Error(nil, "failed to get clique")
	control.Flush()

	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))
	require.Len(t, lines, 3, "V(6) should be suppressed at verbosity 4:\n%s", buf.String())

	// 2026-09-01T16:02:51.179Z\tINFO\tflags/consoleformat_test.go:41\tstarting webhook server
	pattern := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T[\d:.]+Z\t(INFO|DEBUG|ERROR)\t\S+\.go:\d+\t`)
	for _, line := range lines {
		require.Regexp(t, pattern, string(line))
	}

	require.Contains(t, string(lines[0]), "\tINFO\t")
	require.Contains(t, string(lines[1]), "\tDEBUG\t")
	require.Contains(t, string(lines[2]), "\tERROR\t")
}

// TestConsoleFormatVerbosityUpdate checks that SetVerbosityLevel takes effect
// after the logger has been created.
func TestConsoleFormatVerbosityUpdate(t *testing.T) {
	var buf bytes.Buffer
	c := logsapi.NewLoggingConfiguration()
	c.Format = ConsoleLogFormat
	c.Verbosity = 0

	logger, control := consoleFactory{}.Create(*c, logsapi.LoggingOptions{ErrorStream: &buf})

	logger.V(3).Info("hidden at verbosity 0")
	require.Empty(t, buf.String())

	require.NoError(t, control.SetVerbosityLevel(3))
	logger.V(3).Info("visible at verbosity 3")
	control.Flush()
	require.Contains(t, buf.String(), "visible at verbosity 3")
}
