package basispoints

// Temporary local diagnostic. Keep until the fix has been validated; do not publish.
import (
	"bufio"
	"os"
	"path/filepath"
	"time"
)

const localCaptureDirectory = "C:/Users/quantal/workspace/projects/airgate/airgate-core/backend/tmp/bps-live-capture"

type localArgumentCapture struct {
	file               *os.File
	writer             *bufio.Writer
	remaining, pending int
}

func openLocalArgumentCapture(scope string) *localArgumentCapture {
	marker, err := os.Stat(filepath.Join(localCaptureDirectory, "enabled"))
	if err != nil {
		return nil
	}
	if age := time.Since(marker.ModTime()); age < 0 || age > 30*time.Minute {
		return nil
	}
	if len(scope) > 12 {
		scope = scope[:12]
	}
	file, err := os.CreateTemp(localCaptureDirectory, "args-"+scope+"-*.partial")
	if err != nil {
		return nil
	}
	return &localArgumentCapture{file: file, writer: bufio.NewWriterSize(file, 4096), remaining: 64 << 10}
}

func (c *localArgumentCapture) observe(kind string, payload object) {
	if c == nil || c.remaining == 0 {
		return
	}
	if kind != "response.function_call_arguments.delta" && kind != "response.custom_tool_call_input.delta" {
		return
	}
	delta := text(payload["delta"])
	if len(delta) > c.remaining {
		delta = delta[:c.remaining]
	}
	n, err := c.writer.WriteString(delta)
	c.remaining -= n
	c.pending += n
	if err != nil {
		c.remaining = 0
	}
	if c.pending >= 4096 || c.remaining == 0 {
		_ = c.writer.Flush()
		c.pending = 0
	}
}

func (c *localArgumentCapture) close() {
	if c != nil {
		_ = c.writer.Flush()
		_ = c.file.Close()
	}
}
