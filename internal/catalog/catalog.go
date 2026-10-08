package catalog

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Wish struct {
	ID          string
	Name        string
	URL         string
	Description string
	Note        string
	Image       string
	Category    string
}

var safeID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func Load(root string) ([]Wish, error) {
	file, err := os.Open(filepath.Join(root, "data", "wishes.ini"))
	if err != nil {
		return nil, fmt.Errorf("open catalog: %w", err)
	}
	defer file.Close()

	var wishes []Wish
	var current *Wish
	seen := map[string]bool{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 64*1024)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if text == "[wish]" {
			wishes = append(wishes, Wish{})
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

	ids := map[string]bool{}
	for i := range wishes {
		if err := validate(root, wishes[i]); err != nil {
			return nil, fmt.Errorf("wish %d: %w", i+1, err)
		}
		if ids[wishes[i].ID] {
			return nil, fmt.Errorf("wish %d: duplicate id %q", i+1, wishes[i].ID)
		}
		ids[wishes[i].ID] = true
	}
	return wishes, nil
}

func validate(root string, wish Wish) error {
	if !safeID.MatchString(wish.ID) {
		return errors.New("id must contain lowercase letters, numbers, and hyphens")
	}
	if wish.Name == "" || wish.URL == "" || wish.Description == "" || wish.Image == "" || wish.Category == "" {
		return errors.New("id, name, url, description, image, and category are required")
	}
	parsed, err := url.ParseRequestURI(wish.URL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return errors.New("url must be an absolute http or https URL")
	}
	if !strings.HasPrefix(wish.Image, "assets/") || strings.Contains(wish.Image, "..") || strings.ContainsAny(wish.Image, `\`) {
		return errors.New("image must be a safe path below assets/")
	}
	imagePath := filepath.Join(root, "site", filepath.FromSlash(wish.Image))
	info, err := os.Stat(imagePath)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("image %q does not exist", wish.Image)
	}
	return nil
}
