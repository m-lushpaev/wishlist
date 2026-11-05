package server

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"log/slog"
	"mime/multipart"
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

func familyFixture(t *testing.T) (*Server, *store.DB, string) {
	t.Helper()
	root := t.TempDir()
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"test.png", "other.png"} {
		writeTestFile(t, filepath.Join(root, "site", "assets", name), pngData.String())
	}
	writeTestFile(t, filepath.Join(root, "site", "assets", "placeholder.svg"), "<svg xmlns=\"http://www.w3.org/2000/svg\"/>")
	writeTestFile(t, filepath.Join(root, "site", "styles.css"), "body{}")
	writeTestFile(t, filepath.Join(root, "data", "wishes.ini"), familyINI("Maksim Wish", "Anya Wish", "test.png"))
	db, err := store.Open(filepath.Join(root, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app, err := New(Config{ContentRoot: root, PublicHost: "wish.test", AdminHost: "wish-admin.test"}, db)
	if err != nil {
		t.Fatal(err)
	}
	return app, db, root
}

func familyINI(maksim, anya, asset string) string {
	return "[wish]\nid=same\nname=" + maksim + "\nurl=https://example.com/m\ndescription=M\nimage=assets/" + asset + "\ncategory=gear\n[wish]\nprofile=anya\nid=same\nname=" + anya + "\nurl=https://example.com/a\ndescription=A\nimage=assets/" + asset + "\ncategory=books\n"
}

func adminHeaders(group string) map[string]string {
	return map[string]string{"Remote-User": "editor", "Remote-Groups": group, "Origin": "https://wish-admin.test", "Content-Type": "application/x-www-form-urlencoded"}
}

func formFields(t *testing.T, page *httptest.ResponseRecorder) (url.Values, *http.Cookie) {
	t.Helper()
	values := url.Values{}
	for _, key := range []string{"csrf", "profile", "revision", "snapshot"} {
		match := regexp.MustCompile(`name="` + key + `" value="([^"]*)"`).FindStringSubmatch(page.Body.String())
		if len(match) != 2 {
			t.Fatalf("missing %s in %s", key, page.Body.String())
		}
		values.Set(key, match[1])
	}
	cookies := page.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("csrf cookies=%v", cookies)
	}
	return values, cookies[0]
}

func TestPortalProfilesAndAuthorization(t *testing.T) {
	app, _, _ := familyFixture(t)
	h := app.Handler()
	landing := request(t, h, "GET", "/", nil, "wish.test", nil)
	for _, p := range []string{"maksim", "anya", "grisha", "arisha"} {
		if !strings.Contains(landing.Body.String(), `href="/`+p+`"`) {
			t.Fatal("missing profile", p)
		}
	}
	if len(landing.Result().Cookies()) != 0 {
		t.Fatal("landing issued cookies")
	}
	for _, tc := range []struct {
		path, host, group string
		status            int
	}{
		{"/anya", "wish.test", "", 200}, {"/grisha", "wish.test", "", 200}, {"/unknown", "wish.test", "", 404},
		{"/anya/", "wish.test", "", 308}, {"/admin", "wish.test", "admins", 404},
		{"/admin/maksim", "wish-admin.test", "wishlist-anya", 403}, {"/admin/anya", "wish-admin.test", "wishlist-anya", 200},
		{"/admin/anya/new", "wish-admin.test", "users", 403},
	} {
		response := request(t, h, "GET", tc.path, nil, tc.host, adminHeaders(tc.group))
		if response.Code != tc.status {
			t.Errorf("%s %s %s: %d want %d", tc.path, tc.host, tc.group, response.Code, tc.status)
		}
	}
	own := request(t, h, "GET", "/admin", nil, "wish-admin.test", adminHeaders("wishlist-anya"))
	if strings.Contains(own.Body.String(), `href="/admin/maksim"`) || !strings.Contains(own.Body.String(), `href="/admin/anya"`) {
		t.Fatal("profile chooser leaks unauthorized profile")
	}
	public := request(t, h, "GET", "/anya", nil, "wish.test", nil)
	if strings.Contains(public.Body.String(), "Maksim Wish") || !strings.Contains(public.Body.String(), "Anya Wish") {
		t.Fatal("cross-profile content")
	}
	r := httptest.NewRequest("GET", "/admin/anya", nil)
	r.Host = "wish-admin.test"
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set("Remote-User", "forged")
	r.Header.Set("Remote-Groups", "admins")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatal("trusted non-proxy peer")
	}
}

func TestAdminCreateConflictPersistenceAndCrossProfile(t *testing.T) {
	app, _, root := familyFixture(t)
	h := app.Handler()
	headers := adminHeaders("wishlist-anya")
	page := request(t, h, "GET", "/admin/anya/new", nil, "wish-admin.test", headers)
	values, cookie := formFields(t, page)
	for k, v := range map[string]string{"id": "new-gift", "name": "Admin Gift", "url": "https://example.com/gift", "description": "Useful", "category": "books", "image": "assets/placeholder.svg"} {
		values.Set(k, v)
	}
	save := func(path string, v url.Values, heads map[string]string) *httptest.ResponseRecorder {
		return request(t, h, "POST", path, strings.NewReader(v.Encode()), "wish-admin.test", heads, cookie)
	}
	if res := save("/admin/anya/wishes", values, headers); res.Code != 303 {
		t.Fatalf("create=%d %s", res.Code, res.Body.String())
	}
	if res := save("/admin/anya/wishes", values, headers); res.Code != 409 {
		t.Fatalf("duplicate=%d", res.Code)
	}
	if res := save("/admin/maksim/wishes", values, headers); res.Code != 403 {
		t.Fatal("cross-profile write", res.Code)
	}
	for _, origin := range []string{"https://wish.test", "https://wish-admin.test:444", "https://wish-admin.test/", "https://evil.test", "null", ""} {
		bad := adminHeaders("wishlist-anya")
		bad["Origin"] = origin
		if res := save("/admin/anya/wishes", values, bad); res.Code != 403 {
			t.Errorf("origin %q: %d", origin, res.Code)
		}
	}
	badValues := values.Encode()
	noCSRF, _ := url.ParseQuery(badValues)
	noCSRF.Set("csrf", "wrong")
	if res := save("/admin/anya/wishes", noCSRF, headers); res.Code != 403 {
		t.Fatal("CSRF accepted")
	}
	writeTestFile(t, filepath.Join(root, "data", "wishes.ini"), familyINI("Updated Maksim", "Updated Anya", "other.png"))
	public := request(t, h, "GET", "/anya", nil, "wish.test", nil)
	if !strings.Contains(public.Body.String(), "Admin Gift") {
		t.Fatal("git update lost admin content")
	}
	edit := request(t, h, "GET", "/admin/anya/wishes/new-gift/edit", nil, "wish-admin.test", headers)
	update, newCookie := formFields(t, edit)
	cookie = newCookie
	for k, v := range map[string]string{"id": "new-gift", "name": "Updated Admin Gift", "url": "https://example.com/gift", "description": "Useful", "category": "books", "image": "assets/placeholder.svg"} {
		update.Set(k, v)
	}
	if res := save("/admin/anya/wishes/new-gift", update, headers); res.Code != 303 {
		t.Fatalf("update=%d %s", res.Code, res.Body.String())
	}
	update.Set("name", "Stale overwrite")
	if res := save("/admin/anya/wishes/new-gift", update, headers); res.Code != 409 {
		t.Fatal("stale update accepted", res.Code)
	}
	if strings.Contains(request(t, h, "GET", "/anya", nil, "wish.test", nil).Body.String(), "Stale overwrite") {
		t.Fatal("stale data committed")
	}
	app.config.FamilyDisabled = true
	rollback := app.Handler()
	if res := request(t, rollback, "GET", "/anya", nil, "wish.test", nil); res.Code != 404 {
		t.Fatal("rollback exposed profile")
	}
	app.config.FamilyDisabled = false
	if !strings.Contains(request(t, app.Handler(), "GET", "/anya", nil, "wish.test", nil).Body.String(), "Updated Admin Gift") {
		t.Fatal("rollback lost data")
	}
}

func TestGitOverrideImageSurvivesReleaseAndReset(t *testing.T) {
	app, _, root := familyFixture(t)
	h := app.Handler()
	headers := adminHeaders("admins")
	edit := request(t, h, "GET", "/admin/anya/wishes/same/edit", nil, "wish-admin.test", headers)
	values, cookie := formFields(t, edit)
	for k, v := range map[string]string{"id": "same", "name": "Override", "url": "https://example.com/new", "description": "Updated", "category": "gear", "image": "assets/test.png"} {
		values.Set(k, v)
	}
	res := request(t, h, "POST", "/admin/anya/wishes/same", strings.NewReader(values.Encode()), "wish-admin.test", headers, cookie)
	if res.Code != 303 {
		t.Fatalf("save %d %s", res.Code, res.Body.String())
	}
	writeTestFile(t, filepath.Join(root, "data", "wishes.ini"), familyINI("M2", "Git New", "other.png"))
	if err := os.Remove(filepath.Join(root, "site", "assets", "test.png")); err != nil {
		t.Fatal(err)
	}
	public := request(t, h, "GET", "/anya", nil, "wish.test", nil)
	if public.Code != 200 || !strings.Contains(public.Body.String(), "Override") {
		t.Fatal("override lost with git release", public.Body.String())
	}
	mediaPath := regexp.MustCompile(`/media/anya/[a-f0-9]{64}`).FindString(public.Body.String())
	if mediaPath == "" {
		t.Fatal("asset not preserved in database")
	}
	img := request(t, h, "GET", mediaPath, nil, "wish.test", nil)
	if img.Code != 200 || img.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("media=%d %s", img.Code, img.Header())
	}
	edit = request(t, h, "GET", "/admin/anya/wishes/same/edit", nil, "wish-admin.test", headers)
	values, cookie = formFields(t, edit)
	reset := request(t, h, "POST", "/admin/anya/wishes/same/reset", strings.NewReader(values.Encode()), "wish-admin.test", headers, cookie)
	if reset.Code != 303 {
		t.Fatal("reset", reset.Code, reset.Body.String())
	}
	public = request(t, h, "GET", "/anya", nil, "wish.test", nil)
	if !strings.Contains(public.Body.String(), "Git New") || strings.Contains(public.Body.String(), "Override") {
		t.Fatal("reset failed")
	}
}

func TestRemovedWishReservationStillCancelsAndLogsRedactToken(t *testing.T) {
	app, db, root := familyFixture(t)
	h := app.Handler()
	token, err := db.Reserve(context.Background(), "anya", "same", "Snapshot Gift", "Private Friend", "Private Note")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(oldLogger)
	writeTestFile(t, filepath.Join(root, "data", "wishes.ini"), "")
	receipt := request(t, h, "GET", "/reservation/"+token, nil, "wish.test", nil)
	if receipt.Code != 200 || !strings.Contains(receipt.Body.String(), "Snapshot Gift") {
		t.Fatal("deleted gift broke receipt", receipt.Code)
	}
	cookie := receipt.Result().Cookies()[0]
	form := url.Values{"csrf": {cookie.Value}}
	cancel := request(t, h, "POST", "/reservation/"+token+"/cancel", strings.NewReader(form.Encode()), "wish.test", map[string]string{"Origin": "https://wish.test", "Content-Type": "application/x-www-form-urlencoded"}, cookie)
	if cancel.Code != 303 || cancel.Header().Get("Location") != "/anya" {
		t.Fatal("cancel removed gift", cancel.Code)
	}
	for _, secret := range []string{token, "Private Friend", "Private Note"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("secret leaked into logs")
		}
	}
	if strings.Contains(receipt.Body.String(), "Private Friend") || strings.Contains(receipt.Body.String(), "Private Note") {
		t.Fatal("receipt leaks reservation identity")
	}
}

func TestUploadRejectsActiveContentAndForeignProfileReference(t *testing.T) {
	for _, body := range [][]byte{[]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), []byte("<html>evil</html>")} {
		var buffer bytes.Buffer
		writer := multipart.NewWriter(&buffer)
		part, _ := writer.CreateFormFile("photo", "image.png")
		part.Write(body)
		writer.Close()
		req := httptest.NewRequest("POST", "/", &buffer)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		req.ParseMultipartForm(6 * 1024 * 1024)
		if _, err := readUpload(req, "anya"); err == nil {
			t.Fatal("active content accepted")
		}
		removeMultipart(req)
	}
	app, _, _ := familyFixture(t)
	h := app.Handler()
	heads := adminHeaders("wishlist-anya")
	page := request(t, h, "GET", "/admin/anya/new", nil, "wish-admin.test", heads)
	values, cookie := formFields(t, page)
	for k, v := range map[string]string{"id": "x", "name": "X", "url": "https://example.com", "description": "X", "category": "x", "image": "media/maksim/" + strings.Repeat("a", 64)} {
		values.Set(k, v)
	}
	res := request(t, h, "POST", "/admin/anya/wishes", strings.NewReader(values.Encode()), "wish-admin.test", heads, cookie)
	if res.Code != 400 {
		t.Fatal("foreign media accepted", res.Code)
	}
}

func TestMultipartUploadPersistsWithGift(t *testing.T) {
	app, db, _ := familyFixture(t)
	h := app.Handler()
	heads := adminHeaders("wishlist-arisha")
	page := request(t, h, "GET", "/admin/arisha/new", nil, "wish-admin.test", heads)
	values, cookie := formFields(t, page)
	for k, v := range map[string]string{"id": "picture", "name": "Picture Gift", "url": "https://example.com/p", "description": "A gift", "category": "art", "image": "assets/placeholder.svg"} {
		values.Set(k, v)
	}
	var payload, picture bytes.Buffer
	png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 3, 3)))
	writer := multipart.NewWriter(&payload)
	for key, fields := range values {
		writer.WriteField(key, fields[0])
	}
	part, _ := writer.CreateFormFile("photo", "photo.png")
	part.Write(picture.Bytes())
	writer.Close()
	heads["Content-Type"] = writer.FormDataContentType()
	res := request(t, h, "POST", "/admin/arisha/wishes", &payload, "wish-admin.test", heads, cookie)
	if res.Code != 303 {
		t.Fatal("upload failed", res.Code, res.Body.String())
	}
	overrides, err := db.ListOverrides(context.Background(), "arisha")
	if err != nil {
		t.Fatal(err)
	}
	path := overrides["picture"].Wish.Image
	if !strings.HasPrefix(path, "media/arisha/") {
		t.Fatal("upload not stored in db")
	}
	imageResponse := request(t, h, "GET", "/"+path, nil, "wish.test", nil)
	if imageResponse.Code != 200 || !bytes.Equal(imageResponse.Body.Bytes(), picture.Bytes()) {
		t.Fatal("upload changed")
	}
	if request(t, h, "GET", strings.Replace("/"+path, "/arisha/", "/anya/", 1), nil, "wish.test", nil).Code != 404 {
		t.Fatal("cross-profile media accessible")
	}
}
