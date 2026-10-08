package main

import (
	"context"
	"errors"
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
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	config := appserver.Config{
		ContentRoot: env("WISHLIST_CONTENT_ROOT", "."),
		PublicHost:  env("WISHLIST_PUBLIC_HOST", "wish.lushpaev.ru"),
		AdminHost:   env("WISHLIST_ADMIN_HOST", "wish-admin.lushpaev.ru"),
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
