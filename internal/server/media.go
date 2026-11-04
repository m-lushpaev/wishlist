package server

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"github.com/m-lushpaev/wishlist/internal/catalog"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/m-lushpaev/wishlist/internal/profile"
	"github.com/m-lushpaev/wishlist/internal/store"
	_ "golang.org/x/image/webp"
)

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func readUpload(r *http.Request, p string) (*store.Media, error) {
	if r.MultipartForm == nil {
		return nil, nil
	}
	files := r.MultipartForm.File["photo"]
	total := 0
	for _, f := range r.MultipartForm.File {
		total += len(f)
	}
	if total == 0 {
		return nil, nil
	}
	if total != 1 || len(files) != 1 {
		return nil, errors.New("Выберите одно изображение")
	}
	if files[0].Size > 5*1024*1024 {
		return nil, errors.New("Изображение больше 5 MiB")
	}
	file, err := files[0].Open()
	if err != nil {
		return nil, errors.New("Не удалось прочитать изображение")
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, 5*1024*1024+1))
	if err != nil || len(body) > 5*1024*1024 {
		return nil, errors.New("Изображение больше 5 MiB")
	}
	return decodeMedia(body, p)
}

func decodeMedia(body []byte, p string) (*store.Media, error) {
	if len(body) > 5*1024*1024 {
		return nil, errors.New("Изображение больше 5 MiB")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 20_000_000 {
		return nil, errors.New("Нужен корректный JPEG, PNG или WebP до 20 млн пикселей")
	}
	types := map[string]string{"jpeg": "image/jpeg", "png": "image/png", "webp": "image/webp"}
	mime, ok := types[format]
	if !ok {
		return nil, errors.New("Нужен JPEG, PNG или WebP")
	}
	hash := sha256.Sum256(body)
	return &store.Media{Profile: p, Digest: hex.EncodeToString(hash[:]), MIME: mime, Body: body}, nil
}

func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	if hostOnly(r.Host) != s.config.PublicHost && hostOnly(r.Host) != s.config.AdminHost {
		http.NotFound(w, r)
		return
	}
	p, digest := r.PathValue("profile"), r.PathValue("digest")
	if _, ok := profile.Lookup(p); !ok || !digestPattern.MatchString(digest) {
		http.NotFound(w, r)
		return
	}
	media, err := s.database.Media(r.Context(), p, digest)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if media.MIME != "image/jpeg" && media.MIME != "image/png" && media.MIME != "image/webp" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", media.MIME)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", `"`+digest+`"`)
	http.ServeContent(w, r, digest, time.Time{}, bytes.NewReader(media.Body))
}

func captureAsset(root string, wish catalog.Wish) (*store.Media, error) {
	if !strings.HasPrefix(wish.Image, "assets/") || wish.Image == "assets/placeholder.svg" {
		return nil, nil
	}
	if err := catalog.Validate(root, wish, false); err != nil {
		return nil, err
	}
	path := filepath.Join(root, "site", filepath.FromSlash(wish.Image))
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > 5*1024*1024 {
		return nil, errors.New("image too large")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeMedia(body, wish.Profile)
}
