package cli

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	httpapi "github.com/Don-Works/brw/internal/http"
	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/usagelog"
)

type cliUsageWriter struct {
	io.Writer
	bytes  int64
	chars  int64
	binary int64
}

func (w *cliUsageWriter) Write(data []byte) (int, error) {
	n, err := w.Writer.Write(data)
	if n > 0 {
		w.bytes += int64(n)
		chars, binary := usagelog.MeasureJSON(data[:n])
		w.chars += chars
		w.binary += binary
	}
	return n, err
}

func recordCLIUsage(ctx context.Context, controller *httpclient.Controller, v verb, args []string, output *cliUsageWriter, started time.Time, exitCode int, jsonOutput bool, failure error) {
	operation := httpapi.UsageOperation(v.path)
	if operation == "" {
		return
	}
	input, err := json.Marshal(append(strings.Fields(v.name), args...))
	if err != nil {
		return
	}
	inputChars, binary := usagelog.MeasureJSON(input)
	elapsed := time.Since(started)
	outcome := "ok"
	if exitCode != ExitOK {
		outcome = "error"
	}
	event := usagelog.Event{
		Layer: "cli", Operation: operation, Outcome: outcome, Scope: "projection", Representation: "cli_stdout",
		DurationMS: elapsed.Milliseconds(), DurationUS: elapsed.Microseconds(),
		InputBytes: usagelog.Count(int64(len(input))), OutputBytes: usagelog.Count(output.bytes),
		InputTextChars: usagelog.Count(inputChars), OutputTextChars: usagelog.Count(output.chars),
		EstimatedInputTokensChars4: usagelog.Count(usagelog.EstimateTokens(inputChars)), EstimatedOutputTokensChars4: usagelog.Count(usagelog.EstimateTokens(output.chars)),
		BinaryInputBytes: usagelog.Count(binary), BinaryOutputBytes: usagelog.Count(output.binary),
	}
	event.OutputFormat = "human"
	if jsonOutput {
		event.OutputFormat = "json"
	}
	if failure != nil {
		event.ErrorClass = usagelog.SafeErrorClass(httpclient.RemoteClass(failure))
		if event.ErrorClass == "" {
			event.ErrorClass = usagelog.ClassifyError(failure)
		}
		event.ErrorFingerprint = usagelog.Fingerprint(failure.Error())
		event.Retryable = usagelog.Retryable(event.ErrorClass)
		event.HTTPStatus = httpclient.RemoteStatus(failure)
	}
	_ = controller.ReportUsage(ctx, event)
}
