package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/m-lushpaev/wishlist/internal/catalog"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	appserver "github.com/m-lushpaev/wishlist/internal/server"
	"github.com/m-lushpaev/wishlist/internal/store"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "check-content":
			root := env("WISHLIST_CONTENT_ROOT", ".")
			if len(os.Args) == 3 {
				root = os.Args[2]
			} else if len(os.Args) != 2 {
				fmt.Fprintln(os.Stderr, "usage: wishlist check-content [root]")
				os.Exit(2)
			}
			wishes, err := catalog.Load(root)
			if err != nil {
				fmt.Fprintln(os.Stderr, "invalid catalog:", err)
				os.Exit(1)
			}
			fmt.Printf("catalog valid: %d wishes\n", len(wishes))
			return
		case "migrate-db":
			path := env("WISHLIST_DB", "wishlist.db")
			if len(os.Args) == 3 {
				path = os.Args[2]
			} else if len(os.Args) != 2 {
				fmt.Fprintln(os.Stderr, "usage: wishlist migrate-db [path]")
				os.Exit(2)
			}
			db, err := store.Migrate(path)
			if err != nil {
				fmt.Fprintln(os.Stderr, "migration failed:", err)
				os.Exit(1)
			}
			count, err := db.Count(context.Background())
			db.Close()
			if err != nil {
				fmt.Fprintln(os.Stderr, "migration verification failed")
				os.Exit(1)
			}
			fmt.Printf("schema=1 reservations=%d integrity=ok\n", count)
			return
		default:
			fmt.Fprintln(os.Stderr, "unknown command")
			os.Exit(2)
		}
	}
	family, err := strconv.ParseBool(env("WISHLIST_FAMILY_ENABLED", "true"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid WISHLIST_FAMILY_ENABLED")
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	config := appserver.Config{
		FamilyDisabled: !family,
		ContentRoot:    env("WISHLIST_CONTENT_ROOT", "."),
		PublicHost:     env("WISHLIST_PUBLIC_HOST", "wish.lushpaev.ru"),
		AdminHost:      env("WISHLIST_ADMIN_HOST", "wish-admin.lushpaev.ru"),
	}
	database, err := store.Open(env("WISHLIST_DB", "wishlist.db"))
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer database.Close()
	application, err := appserver.New(config, database)
	if err != nil {
		slog.Error("initialize application", "error", err)
		os.Exit(1)
	}
	server := &http.Server{
		Addr:              env("WISHLIST_LISTEN", "127.0.0.1:10087"),
		Handler:           application.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	go func() {
		slog.Info("wishlist started", "version", version, "listen", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server stopped", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		slog.Error("shutdown failed", "error", err)
		os.Exit(1)
	}
	slog.Info("wishlist stopped")
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
