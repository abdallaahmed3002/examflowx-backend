package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
)

var uploadNamePattern = regexp.MustCompile(`^[a-f0-9]{32}\.(png|jpg|jpeg|webp|gif)$`)

var imageMagic = []struct {
	magic []byte
	ext   string
}{
	{[]byte("\x89PNG\r\n\x1a\n"), "png"},
	{[]byte("\xff\xd8\xff"), "jpg"},
	{[]byte("GIF87a"), "gif"},
	{[]byte("GIF89a"), "gif"},
}

func uploadDir() string {
	if d := os.Getenv("UPLOAD_DIR"); d != "" {
		return d
	}
	return "uploads"
}

// uploadImage accepts a single image (≤2MB), verifies real image content by
// magic bytes, and stores it under a random hex name — never the client name,
// so path traversal and overwrite are impossible.
func (s *server) uploadImage(w http.ResponseWriter, r *http.Request, teacherID string) {
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		fail(w, 400, "Multipart form with one 'image' file required")
		return
	}
	files := r.MultipartForm.File["image"]
	if len(files) != 1 {
		fail(w, 400, "Exactly one image file required")
		return
	}
	fh := files[0]
	if fh.Size > 2<<20 {
		fail(w, 413, "Image must be 2MB or smaller")
		return
	}
	f, err := fh.Open()
	if err != nil {
		fail(w, 400, "Unable to read image")
		return
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	head := buf[:n]
	ext := ""
	for _, m := range imageMagic {
		if bytes.HasPrefix(head, m.magic) {
			ext = m.ext
			break
		}
	}
	if ct := http.DetectContentType(head); ext == "" || !strings.HasPrefix(ct, "image/") {
		fail(w, 415, "File is not a supported image (PNG, JPEG, GIF or WebP)")
		return
	}
	if _, err := f.Seek(0, 0); err != nil {
		fail(w, 500, "Unable to store image")
		return
	}
	if err := os.MkdirAll(uploadDir(), 0o755); err != nil {
		fail(w, 500, "Unable to store image")
		return
	}
	name := token()[:32] + "." + ext // token() is 64 hex chars; take 32
	dst, err := os.OpenFile(filepath.Join(uploadDir(), name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		fail(w, 500, "Unable to store image")
		return
	}
	defer dst.Close()
	if _, err := dst.ReadFrom(f); err != nil {
		fail(w, 500, "Unable to store image")
		return
	}
	s.audit(teacherID, "upload.image", "upload", "")
	write(w, 201, map[string]string{"url": "/uploads/" + name})
}

// serveUpload only ever serves validated random names from the upload dir.
func (s *server) serveUpload(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "file")
	if !uploadNamePattern.MatchString(name) {
		fail(w, 404, "Not found")
		return
	}
	path := filepath.Join(uploadDir(), name)
	if _, err := os.Stat(path); err != nil {
		fail(w, 404, "Not found")
		return
	}
	// Content-Type is derived from the stored (magic-verified) extension only.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, path)
}
