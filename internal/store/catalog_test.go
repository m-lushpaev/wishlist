package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-lushpaev/wishlist/internal/catalog"
)

func TestOverrideCASPersistenceAndAtomicMedia(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	digest := strings.Repeat("a", 64)
	wish := catalog.Wish{Profile: "anya", ID: "bike", Name: "A", Image: "media/anya/" + digest}
	media := &Media{Profile: "anya", Digest: digest, MIME: "image/png", Body: []byte("image")}
	if err = d.SaveOverride(ctx, Override{Wish: wish}, 0, media); err != nil {
		t.Fatal(err)
	}
	rows, err := d.ListOverrides(ctx, "anya")
	if err != nil {
		t.Fatal(err)
	}
	first := rows["bike"].Revision
	wish.Name = "B"
	if err = d.SaveOverride(ctx, Override{Wish: wish}, first, nil); err != nil {
		t.Fatal(err)
	}
	if err = d.SaveOverride(ctx, Override{Wish: wish}, first, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale save=%v", err)
	}
	second, _ := d.ListOverrides(ctx, "anya")
	if err = d.ResetOverride(ctx, "anya", "bike", second["bike"].Revision); err != nil {
		t.Fatal(err)
	}
	if err = d.SaveOverride(ctx, Override{Wish: wish}, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err = d.SaveOverride(ctx, Override{Wish: wish}, first, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("ABA stale save=%v", err)
	}
	fresh := &Media{Profile: "anya", Digest: strings.Repeat("b", 64), MIME: "image/png", Body: []byte("new")}
	wish.Image = "media/anya/" + fresh.Digest
	if err = d.SaveOverride(ctx, Override{Wish: wish}, first, fresh); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err = d.Media(ctx, "anya", fresh.Digest); err == nil {
		t.Fatal("failed save left media")
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.ListOverrides(ctx, "anya")
	if err != nil || got["bike"].Wish.Name != "B" {
		t.Fatalf("reopen=%#v err=%v", got, err)
	}
	other, err := d.ListOverrides(ctx, "maksim")
	if err != nil || len(other) != 0 {
		t.Fatal("cross-profile override", err)
	}
	if _, err = d.Media(ctx, "maksim", digest); err == nil {
		t.Fatal("cross-profile media")
	}
}
