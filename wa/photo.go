package wa

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// maxPhotoBytes caps a download; WhatsApp profile pictures are under 1 MB.
const maxPhotoBytes = 10 << 20

// httpClient has a timeout so a stalled CDN download cannot hang a request.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// PhotoDir is where the flow saves profile pictures: WABOT_PHOTO_DIR if set,
// else a wabot folder in the system temp directory.
func PhotoDir() string {
	if dir := os.Getenv("WABOT_PHOTO_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), "wabot-photos")
}

// Fetch downloads a URL into memory. Profile picture URLs are signed and
// expire, so they must be fetched soon after the lookup that returned them.
func Fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	// Close errors after a full read carry no useful information.
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxPhotoBytes))
}

// SaveProfilePhoto downloads a profile picture to dir/profile_<phone>.jpg and
// returns the absolute path. Naming by phone keeps one file per contact.
func SaveProfilePhoto(ctx context.Context, url, dir, phone string) (string, error) {
	data, err := Fetch(ctx, url)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(dir, "profile_"+phone+".jpg"))
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, data, 0o644)
}
