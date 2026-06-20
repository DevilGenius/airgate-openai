package imgen

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDetectMimeTypeDimensionsAndExtension(t *testing.T) {
	pngData := tinyPNG(t)
	if got := detectMimeType(pngData); got != "image/png" {
		t.Fatalf("png mime = %q", got)
	}
	if got := detectMimeType([]byte{0xFF, 0xD8, 0x00, 0x00}); got != "image/jpeg" {
		t.Fatalf("jpeg mime = %q", got)
	}
	if got := detectMimeType([]byte("GIF89a")); got != "image/gif" {
		t.Fatalf("gif mime = %q", got)
	}
	if got := detectMimeType([]byte("RIFFxxxxWEBP")); got != "image/webp" {
		t.Fatalf("webp mime = %q", got)
	}
	if got := detectMimeType([]byte("unknown")); got != "image/png" {
		t.Fatalf("default mime = %q", got)
	}

	w, h := getImageDimensions(pngData)
	if w != 2 || h != 3 {
		t.Fatalf("png dimensions = %dx%d", w, h)
	}
	headerOnly := make([]byte, 24)
	copy(headerOnly, []byte{0x89, 'P', 'N', 'G'})
	headerOnly[19] = 7
	headerOnly[23] = 9
	w, h = getImageDimensions(headerOnly)
	if w != 7 || h != 9 {
		t.Fatalf("fallback dimensions = %dx%d", w, h)
	}
	w, h = getImageDimensions([]byte("bad"))
	if w != 0 || h != 0 {
		t.Fatalf("bad dimensions = %dx%d", w, h)
	}

	for mime, want := range map[string]string{
		"image/jpeg": ".jpg",
		"image/gif":  ".gif",
		"image/webp": ".webp",
		"image/png":  ".png",
		"text/plain": ".png",
	} {
		if got := mimeToExt(mime); got != want {
			t.Fatalf("mimeToExt(%q) = %q, want %q", mime, got, want)
		}
	}
}

func TestUploadFileFullFlowIgnoresConfirmFailure(t *testing.T) {
	pngData := tinyPNG(t)
	var sawFinalize bool
	c := newTestClient(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host + req.URL.Path {
		case "chatgpt.com/backend-api/files":
			body, _ := io.ReadAll(req.Body)
			if !bytes.Contains(body, []byte(`"file_name":"image.png"`)) {
				t.Fatalf("fileCreate body = %s", body)
			}
			return imgenHTTPResponse(http.StatusOK, `{"file_id":"file_1","upload_url":"https://uploads.example.test/blob"}`, nil), nil
		case "uploads.example.test/blob":
			if req.Method != http.MethodPut {
				t.Fatalf("upload method = %s", req.Method)
			}
			if req.Header.Get("User-Agent") != DefaultUA {
				t.Fatalf("external upload UA = %q", req.Header.Get("User-Agent"))
			}
			return imgenHTTPResponse(http.StatusCreated, "", nil), nil
		case "chatgpt.com/backend-api/files/file_1/uploaded":
			sawFinalize = true
			return imgenHTTPResponse(http.StatusOK, "", nil), nil
		case "chatgpt.com/backend-api/files/download/file_1":
			return imgenHTTPResponse(http.StatusInternalServerError, "", nil), nil
		default:
			t.Fatalf("unexpected request %s", req.URL)
			return nil, nil
		}
	})

	uploaded, err := c.uploadFile(ImageInput{Data: pngData})
	if err != nil {
		t.Fatalf("uploadFile returned err: %v", err)
	}
	if !sawFinalize {
		t.Fatal("external upload should finalize file")
	}
	if uploaded.FileID != "file_1" || uploaded.FileName != "image.png" || uploaded.MimeType != "image/png" {
		t.Fatalf("uploaded metadata = %#v", uploaded)
	}
	if uploaded.Width != 2 || uploaded.Height != 3 || uploaded.Size != int64(len(pngData)) {
		t.Fatalf("uploaded dimensions/size = %#v", uploaded)
	}
}

func TestUploadFileErrorsAndUploadVariants(t *testing.T) {
	if _, err := (&Client{}).uploadFile(ImageInput{}); err == nil {
		t.Fatal("empty image upload should fail")
	}

	c := newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusOK, `{}`, nil), nil
	})
	if _, _, err := c.fileCreate("x.png", 1, "image/png"); err == nil {
		t.Fatal("fileCreate without file_id should fail")
	}

	c = newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusBadRequest, "bad", nil), nil
	})
	if _, _, err := c.fileCreate("x.png", 1, "image/png"); err == nil {
		t.Fatal("fileCreate non-200 should fail")
	}

	c = newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusOK, `{bad`, nil), nil
	})
	if _, _, err := c.fileCreate("x.png", 1, "image/png"); err == nil {
		t.Fatal("fileCreate bad JSON should fail")
	}

	var sawStream bool
	c = newTestClient(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/backend-api/files/process_upload_stream" {
			t.Fatalf("unexpected stream request %s", req.URL)
		}
		sawStream = true
		if !strings.HasPrefix(req.Header.Get("Content-Type"), "multipart/form-data") {
			t.Fatalf("stream content type = %q", req.Header.Get("Content-Type"))
		}
		return imgenHTTPResponse(http.StatusOK, "", nil), nil
	})
	if err := c.fileUploadData("file_2", "", []byte("data"), "image/png"); err != nil {
		t.Fatalf("fileUploadData stream returned err: %v", err)
	}
	if !sawStream {
		t.Fatal("stream upload path not called")
	}

	c = newTestClient(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/backend-api/files/process_upload_stream" {
			t.Fatalf("unexpected relative upload request %s", req.URL)
		}
		if req.Method != http.MethodPost {
			t.Fatalf("relative upload method = %s", req.Method)
		}
		if req.Header.Get("X-File-Id") != "file_3" {
			t.Fatalf("relative upload X-File-Id = %q", req.Header.Get("X-File-Id"))
		}
		return imgenHTTPResponse(http.StatusOK, "", nil), nil
	})
	if err := c.fileUploadData("file_3", "/backend-api/files/process_upload_stream", []byte("data"), "image/png"); err != nil {
		t.Fatalf("relative upload returned err: %v", err)
	}

	c = newTestClient(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host + req.URL.Path {
		case "abc.oaiusercontent.com/blob":
			if req.Header.Get("x-ms-blob-type") != "BlockBlob" {
				t.Fatalf("azure blob header missing: %#v", req.Header)
			}
			return imgenHTTPResponse(http.StatusOK, "", nil), nil
		case "chatgpt.com/backend-api/files/file_4/uploaded":
			return imgenHTTPResponse(http.StatusOK, "", nil), nil
		default:
			t.Fatalf("unexpected azure upload request %s", req.URL)
		}
		return nil, nil
	})
	if err := c.fileUploadToURL("https://abc.oaiusercontent.com/blob", strings.NewReader("x"), "image/png", "file_4"); err != nil {
		t.Fatalf("azure upload returned err: %v", err)
	}
}

func TestDownloadImagePaths(t *testing.T) {
	c := newTestClient(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host + req.URL.Path {
		case "chatgpt.com/backend-api/files/download/file_1":
			if req.URL.Query().Get("conversation_id") != "conv 1" {
				t.Fatalf("conversation_id query = %q", req.URL.RawQuery)
			}
			return imgenHTTPResponse(http.StatusFound, "", http.Header{"Location": []string{"https://cdn.example.test/image.png"}}), nil
		case "cdn.example.test/image.png":
			if req.Header.Get("Accept") != "image/*,*/*;q=0.8" {
				t.Fatalf("cdn accept = %q", req.Header.Get("Accept"))
			}
			return imgenHTTPResponse(http.StatusOK, "PNGDATA", nil), nil
		default:
			t.Fatalf("unexpected request %s", req.URL)
			return nil, nil
		}
	})
	data, err := c.downloadImage("conv 1", "file-service://file_1")
	if err != nil {
		t.Fatalf("downloadImage file-service returned err: %v", err)
	}
	if string(data) != "PNGDATA" {
		t.Fatalf("download data = %q", data)
	}
}

func TestDownloadImageFallbacksAndErrors(t *testing.T) {
	c := newTestClient(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host + req.URL.Path {
		case "chatgpt.com/backend-api/files/download/file_json":
			return imgenHTTPResponse(http.StatusOK, `{"download_url":"https://cdn.example.test/json.png"}`, nil), nil
		case "chatgpt.com/backend-api/files/download/file_direct":
			return imgenHTTPResponse(http.StatusOK, `DIRECT`, nil), nil
		case "chatgpt.com/backend-api/files/download/file_old":
			return imgenHTTPResponse(http.StatusInternalServerError, "nope", nil), nil
		case "chatgpt.com/backend-api/files/file_old/download":
			return imgenHTTPResponse(http.StatusOK, `{"download_url":"https://cdn.example.test/old.png"}`, nil), nil
		case "chatgpt.com/backend-api/conversation/conv/attachment/sed_1/download":
			return imgenHTTPResponse(http.StatusOK, `{"download_url":"https://cdn.example.test/sed.png"}`, nil), nil
		case "cdn.example.test/json.png":
			return imgenHTTPResponse(http.StatusOK, "JSON", nil), nil
		case "cdn.example.test/old.png":
			return imgenHTTPResponse(http.StatusOK, "OLD", nil), nil
		case "cdn.example.test/sed.png":
			return imgenHTTPResponse(http.StatusOK, "SED", nil), nil
		default:
			t.Fatalf("unexpected request %s", req.URL)
			return nil, nil
		}
	})

	for ref, want := range map[string]string{
		"file-service://file_json":   "JSON",
		"file-service://file_direct": "DIRECT",
		"file-service://file_old":    "OLD",
		"sediment://sed_1":           "SED",
	} {
		got, err := c.downloadImage("conv", ref)
		if err != nil {
			t.Fatalf("downloadImage(%q) returned err: %v", ref, err)
		}
		if string(got) != want {
			t.Fatalf("downloadImage(%q) = %q, want %q", ref, got, want)
		}
	}

	if _, err := c.downloadImage("conv", "unknown://id"); err == nil {
		t.Fatal("unknown ref should fail")
	}
	c = newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusOK, `{}`, nil), nil
	})
	if _, err := c.downloadByJSONLink("/empty"); err == nil {
		t.Fatal("empty download URL should fail")
	}
	c = newTestClient(func(req *http.Request) (*http.Response, error) {
		return imgenHTTPResponse(http.StatusForbidden, "denied", nil), nil
	})
	if _, err := c.fetchBinary("https://cdn.example.test/denied"); err == nil {
		t.Fatal("fetchBinary non-200 should fail")
	}
}
