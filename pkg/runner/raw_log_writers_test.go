package runner

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestRawLogWritersKeepStdoutAndStderrSeparate(t *testing.T) {
	var output bytes.Buffer
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	logger.SetOutput(&output)
	logger.SetLevel(logrus.DebugLevel)
	var handled []string
	command := func(line string) bool {
		handled = append(handled, line)
		return true
	}
	stdout, stderr := rawLogWriters(logger, command, true)
	if _, err := stdout.Write([]byte("out\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.Write([]byte("err\n")); err != nil {
		t.Fatal(err)
	}
	var records []struct {
		Message   string `json:"msg"`
		RawOutput bool   `json:"raw_output"`
		RawStream string `json:"raw_stream"`
	}
	decoder := json.NewDecoder(&output)
	for decoder.More() {
		var record struct {
			Message   string `json:"msg"`
			RawOutput bool   `json:"raw_output"`
			RawStream string `json:"raw_stream"`
		}
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if len(records) != 2 || records[0].RawStream != "stdout" || records[1].RawStream != "stderr" ||
		!records[0].RawOutput || !records[1].RawOutput || records[0].Message != "out\n" || records[1].Message != "err\n" {
		t.Fatalf("unexpected structured output: %+v", records)
	}
	if len(handled) != 2 || handled[0] != "out\n" || handled[1] != "err\n" {
		t.Fatalf("command handler received: %q", handled)
	}
}
