package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const imageKeepAliveInterval = 30 * time.Second

func writeSSEPing(w http.ResponseWriter) error {
	return writeImageSSEBytes(w, []byte("event: ping\ndata: {}\n\n"))
}

func writeImageSSEBytes(w http.ResponseWriter, data []byte) error {
	n, err := w.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func writeSSEData(w http.ResponseWriter, data []byte) error {
	for _, part := range [][]byte{[]byte("data: "), data, []byte("\n\n")} {
		n, err := w.Write(part)
		if err != nil {
			return err
		}
		if n != len(part) {
			return io.ErrShortWrite
		}
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func writeSSEDone(w http.ResponseWriter) error {
	return writeImageSSEBytes(w, []byte("data: [DONE]\n\n"))
}

func writeSSEError(w http.ResponseWriter, message string) error {
	if message != imageTooLargeSSEErrorMessage {
		message = sanitizedImageSSEErrorMessage
	}
	errEvent, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": "server_error"},
	})
	if err := writeSSEData(w, errEvent); err != nil {
		return err
	}
	return writeSSEDone(w)
}

func writeImagesRESTSSE(w http.ResponseWriter, body []byte, isEdit bool) error {
	sdk.BeginStreamCompletion(w)
	events := imagesRESTStreamCompletedEvents(body, isEdit)
	if len(events) == 0 {
		if err := writeSSEData(w, body); err != nil {
			return err
		}
	} else {
		for _, event := range events {
			if len(event) > maxResponseEventBytes {
				return errResponseTooLarge
			}
			if err := writeSSEData(w, event); err != nil {
				return err
			}
		}
	}
	return writeSSEDone(w)
}
