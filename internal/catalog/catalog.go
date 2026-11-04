package catalog

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/m-lushpaev/wishlist/internal/profile"
)

type Key struct{ Profile, ID string }

type Snapshot struct {
	Root, Revision string
	Wishes         []Wish
}

type Wish struct {
	Profile     string
	ID          string
	Name        string
	URL         string
	Description string
	Note        string
	Image       string
	Category    string
}

var mediaPath = regexp.MustCompile(`^media/(maksim|anya|grisha|arisha)/[a-f0-9]{64}$`)

var safeID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func Load(root string) ([]Wish, error) {
	snapshot, err := LoadSnapshot(root)
	return snapshot.Wishes, err
}

func LoadSnapshot(root string) (Snapshot, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return Snapshot{}, err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return Snapshot{}, err
	}
	data, err := os.ReadFile(filepath.Join(resolved, "data", "wishes.ini"))
	if err != nil {
		return Snapshot{}, fmt.Errorf("open catalog: %w", err)
	}
	if len(data) > 4*1024*1024 {
		return Snapshot{}, errors.New("catalog too large")
	}
	wishes, err := parse(resolved, data)
	hash := sha256.Sum256(data)
	return Snapshot{Root: resolved, Revision: hex.EncodeToString(hash[:]), Wishes: wishes}, err
}

func parse(root string, data []byte) ([]Wish, error) {
	var wishes []Wish
	var current *Wish
	seen := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 64*1024)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if text == "[wish]" {
			wishes = append(wishes, Wish{Profile: "maksim"})
			current = &wishes[len(wishes)-1]
			continue
		}
		if current == nil {
			return nil, fmt.Errorf("line %d: field before [wish]", line)
		}
		key, value, found := strings.Cut(text, "=")
		if !found {
			return nil, fmt.Errorf("line %d: expected key=value", line)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		marker := fmt.Sprintf("%d:%s", len(wishes), key)
		if seen[marker] {
			return nil, fmt.Errorf("line %d: duplicate field %q", line, key)
		}
		seen[marker] = true
		switch key {
		case "profile":
			if value != "" {
				current.Profile = value
			}
		case "id":
			current.ID = value
		case "name":
			current.Name = value
		case "url":
			current.URL = value
		case "description":
			current.Description = value
		case "note":
			current.Note = value
		case "image":
			current.Image = value
		case "category":
			current.Category = value
		default:
			return nil, fmt.Errorf("line %d: unknown field %q", line, key)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}

	ids := map[Key]bool{}
	for i := range wishes {
		if err := Validate(root, wishes[i], false); err != nil {
			return nil, fmt.Errorf("wish %d: %w", i+1, err)
		}
		if ids[Key{wishes[i].Profile, wishes[i].ID}] {
			return nil, fmt.Errorf("wish %d: duplicate id %q", i+1, wishes[i].ID)
		}
		ids[Key{wishes[i].Profile, wishes[i].ID}] = true
	}
	return wishes, nil
}

func Validate(root string, wish Wish, allowMedia bool) error {
	if _, ok := profile.Lookup(wish.Profile); !ok {
		return errors.New("unknown profile")
	}
	for _, field := range []struct {
		text  string
		limit int
	}{{wish.Name, 200}, {wish.Description, 4000}, {wish.Note, 2000}, {wish.Category, 80}} {
		if !utf8.ValidString(field.text) || utf8.RuneCountInString(field.text) > field.limit {
			return errors.New("text field too long or invalid UTF-8")
		}
		for _, r := range field.text {
			if unicode.IsControl(r) && r != '\n' && r != '\t' {
				return errors.New("invalid text character")
			}
		}
	}
	if len(wish.URL) > 2048 || strings.ContainsFunc(wish.URL, unicode.IsControl) {
		return errors.New("invalid URL")
	}
	if !safeID.MatchString(wish.ID) {
		return errors.New("id must contain lowercase letters, numbers, and hyphens")
	}
	if wish.Name == "" || wish.URL == "" || wish.Description == "" || wish.Image == "" || wish.Category == "" {
		return errors.New("id, name, url, description, image, and category are required")
	}
	parsed, err := url.ParseRequestURI(wish.URL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return errors.New("url must be an absolute http or https URL")
	}
	if allowMedia && wish.Image == "assets/placeholder.svg" {
		return nil
	}
	if allowMedia && mediaPath.MatchString(wish.Image) {
		if !strings.HasPrefix(wish.Image, "media/"+wish.Profile+"/") {
			return errors.New("image belongs to another profile")
		}
		return nil
	}
	if !strings.HasPrefix(wish.Image, "assets/") || strings.Contains(wish.Image, "..") || strings.ContainsAny(wish.Image, `\?#%`) {
		return errors.New("image must be a safe path below assets/")
	}
	imagePath := filepath.Join(root, "site", filepath.FromSlash(wish.Image))
	resolvedImage, err := filepath.EvalSymlinks(imagePath)
	if err != nil {
		return fmt.Errorf("image %q does not exist", wish.Image)
	}
	relative, err := filepath.Rel(filepath.Join(root, "site", "assets"), resolvedImage)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return errors.New("image escapes assets")
	}
	info, err := os.Stat(resolvedImage)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("image %q does not exist", wish.Image)
	}
	return nil
}
