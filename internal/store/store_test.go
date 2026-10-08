package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestReservationLifecycle(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "wishlist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	token, err := database.Reserve(ctx, "bike", "Friend", "Size M")
	if err != nil || token == "" {
		t.Fatalf("reserve: token=%q err=%v", token, err)
	}
	if _, err := database.Reserve(ctx, "bike", "Other", ""); !errors.Is(err, ErrReserved) {
		t.Fatalf("duplicate reserve: %v", err)
	}
	reservation, err := database.FindByToken(ctx, token)
	if err != nil || reservation.WishID != "bike" || reservation.Name != "Friend" {
		t.Fatalf("find: %#v err=%v", reservation, err)
	}
	if removed, err := database.Cancel(ctx, token); err != nil || !removed {
		t.Fatalf("cancel: removed=%v err=%v", removed, err)
	}
	if removed, err := database.Cancel(ctx, token); err != nil || removed {
		t.Fatalf("second cancel: removed=%v err=%v", removed, err)
	}
}
