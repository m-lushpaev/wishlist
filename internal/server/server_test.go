package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/m-lushpaev/wishlist/internal/store"
)

func TestReserveConflictAndCancel(t *testing.T) {
	handler, cleanup := testHandler(t)
	defer cleanup()

	home := request(t, handler, http.MethodGet, "/", nil, "wish.test", nil)
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), "Test Wish") {
		t.Fatalf("home: status=%d body=%s", home.Code, home.Body.String())
	}
	cookie := home.Result().Cookies()[0]
	form := url.Values{"csrf": {cookie.Value}, "name": {"Friend"}, "note": {"Keep secret"}}
	headers := map[string]string{"Origin": "https://wish.test", "HX-Request": "true", "Content-Type": "application/x-www-form-urlencoded"}
	reserved := request(t, handler, http.MethodPost, "/reserve/test-wish", strings.NewReader(form.Encode()), "wish.test", headers, cookie)
	if reserved.Code != http.StatusOK || !strings.Contains(reserved.Body.String(), "секретную ссылку") {
		t.Fatalf("reserve: status=%d body=%s", reserved.Code, reserved.Body.String())
	}
	tokenMatch := regexp.MustCompile(`/reservation/([A-Za-z0-9_-]+)`).FindStringSubmatch(reserved.Body.String())
	if len(tokenMatch) != 2 {
		t.Fatalf("missing cancellation token: %s", reserved.Body.String())
	}

	conflict := request(t, handler, http.MethodPost, "/reserve/test-wish", strings.NewReader(form.Encode()), "wish.test", headers, cookie)
	if conflict.Code != http.StatusOK || !strings.Contains(conflict.Body.String(), "уже зарезервирован") {
		t.Fatalf("conflict: status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	cancelled := request(t, handler, http.MethodPost, "/reservation/"+tokenMatch[1]+"/cancel", strings.NewReader(form.Encode()), "wish.test", headers, cookie)
	if cancelled.Code != http.StatusSeeOther || cancelled.Header().Get("Location") != "/" {
		t.Fatalf("cancel: status=%d location=%q", cancelled.Code, cancelled.Header().Get("Location"))
	}
}

func TestAdminRequiresGroupAndHost(t *testing.T) {
	handler, cleanup := testHandler(t)
	defer cleanup()
	forbidden := request(t, handler, http.MethodGet, "/admin", nil, "wish-admin.test", nil)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("without group: %d", forbidden.Code)
	}
	allowed := request(t, handler, http.MethodGet, "/admin", nil, "wish-admin.test", map[string]string{"Remote-User": "maks", "Remote-Groups": "users,admins"})
	if allowed.Code != http.StatusOK || !strings.Contains(allowed.Body.String(), "Состояние wishlist") {
		t.Fatalf("admin: status=%d body=%s", allowed.Code, allowed.Body.String())
	}
	public := request(t, handler, http.MethodGet, "/admin", nil, "wish.test", map[string]string{"Remote-Groups": "admins"})
	if public.Code != http.StatusNotFound {
		t.Fatalf("public host exposed admin: %d", public.Code)
	}
}

func testHandler(t *testing.T) (http.Handler, func()) {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "site", "assets", "test.jpg"), "photo")
	writeTestFile(t, filepath.Join(root, "site", "styles.css"), "body{}")
	writeTestFile(t, filepath.Join(root, "data", "wishes.ini"), `[wish]
id=test-wish
name=Test Wish
url=https://example.com/test
description=Testing.
note=
image=assets/test.jpg
category=test
`)
	database, err := store.Open(filepath.Join(root, "wishlist.db"))
	if err != nil {
		t.Fatal(err)
	}
	application, err := New(Config{ContentRoot: root, PublicHost: "wish.test", AdminHost: "wish-admin.test"}, database)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	return application.Handler(), func() { database.Close() }
}

func request(t *testing.T, handler http.Handler, method, target string, body io.Reader, host string, headers map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, body)
	request.Host = host
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
