package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "site", "assets", "thing.jpg"), "photo")
	mustWrite(t, filepath.Join(root, "data", "wishes.ini"), `[wish]
id=good-thing
name=Good Thing
url=https://example.com/thing
description=Useful.
note=Blue.
image=assets/thing.jpg
category=gear
`)
	wishes, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(wishes) != 1 || wishes[0].ID != "good-thing" || wishes[0].Category != "gear" {
		t.Fatalf("unexpected wishes: %#v", wishes)
	}
}

func TestRejectsUnsafeAndDuplicateData(t *testing.T) {
	for name, data := range map[string]string{
		"unsafe id":  "[wish]\nid=../bad\nname=x\nurl=https://example.com\ndescription=x\nimage=assets/x.jpg\ncategory=x\n",
		"unsafe url": "[wish]\nid=x\nname=x\nurl=javascript:alert(1)\ndescription=x\nimage=assets/x.jpg\ncategory=x\n",
		"duplicate":  "[wish]\nid=x\nname=x\nname=y\nurl=https://example.com\ndescription=x\nimage=assets/x.jpg\ncategory=x\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			mustWrite(t, filepath.Join(root, "site", "assets", "x.jpg"), "photo")
			mustWrite(t, filepath.Join(root, "data", "wishes.ini"), data)
			if _, err := Load(root); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
