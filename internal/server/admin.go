package server

import (
	"database/sql"
	"errors"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/m-lushpaev/wishlist/internal/catalog"
	"github.com/m-lushpaev/wishlist/internal/profile"
	"github.com/m-lushpaev/wishlist/internal/store"
)

type adminReservation struct {
	WishName, WishID string
	CreatedAt        time.Time
}
type adminView struct {
	User                   string
	Profiles               []profile.Profile
	Profile                profile.Profile
	Entries                []entry
	TotalWishes, Available int
	Reservations           []adminReservation
	CSRF, Snapshot         string
}

type adminFormView struct {
	User                          string
	Profile                       profile.Profile
	Wish                          catalog.Wish
	Revision                      int64
	Snapshot, CSRF, Action, Error string
	IsNew, HasGit, Hidden         bool
	Assets                        []string
}

func (s *Server) canEdit(r *http.Request, p string) bool {
	return s.enabled(p) && (hasGroup(r.Header.Get("Remote-Groups"), "admins") || hasGroup(r.Header.Get("Remote-Groups"), "wishlist-"+p))
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request, p string) bool {
	w.Header().Set("Cache-Control", "no-store")
	if !s.requireHost(w, r, s.config.AdminHost) {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	user := strings.TrimSpace(r.Header.Get("Remote-User"))
	if user == "" || len(user) > 120 {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	if p != "" {
		if !s.enabled(p) {
			http.NotFound(w, r)
			return false
		}
		if !s.canEdit(r, p) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return false
		}
		return true
	}
	for _, p := range profile.All() {
		if s.canEdit(r, p.Slug) {
			return true
		}
	}
	http.Error(w, "Forbidden", http.StatusForbidden)
	return false
}

func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r, "") {
		return
	}
	view := adminView{User: r.Header.Get("Remote-User")}
	for _, p := range profile.All() {
		if s.canEdit(r, p.Slug) {
			view.Profiles = append(view.Profiles, p)
		}
	}
	s.execute(w, r, "admin.html", view)
}

func (s *Server) adminProfile(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("profile")
	if !s.authorize(w, r, p) {
		return
	}
	data, err := s.effective(r.Context(), p)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	reservations, err := s.database.List(r.Context(), p)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	prof, _ := profile.Lookup(p)
	view := adminView{User: r.Header.Get("Remote-User"), Profile: prof, Entries: data.Entries, CSRF: ensureCSRF(w, r), Snapshot: data.Snapshot.Revision}
	names := map[string]string{}
	for _, e := range data.Entries {
		names[e.Wish.ID] = e.Wish.Name
		if !e.Hidden {
			view.TotalWishes++
			if _, ok := reservations[e.Wish.ID]; !ok {
				view.Available++
			}
		}
	}
	for _, reservation := range reservations {
		name := names[reservation.WishID]
		if name == "" {
			name = reservation.WishName
		}
		if name == "" {
			name = reservation.WishID
		}
		view.Reservations = append(view.Reservations, adminReservation{WishName: name, WishID: reservation.WishID, CreatedAt: reservation.CreatedAt})
	}
	sort.Slice(view.Reservations, func(i, j int) bool {
		a, b := view.Reservations[i], view.Reservations[j]
		if a.CreatedAt.Equal(b.CreatedAt) {
			return a.WishID < b.WishID
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	s.execute(w, r, "admin-profile.html", view)
}

func (s *Server) newForm(r *http.Request, data profileCatalog, e entry, isNew bool) adminFormView {
	prof, _ := profile.Lookup(r.PathValue("profile"))
	view := adminFormView{User: r.Header.Get("Remote-User"), Profile: prof, Wish: e.Wish, Revision: e.Revision, Snapshot: data.Snapshot.Revision, IsNew: isNew, HasGit: e.HasGit, Hidden: e.Hidden}
	view.Action = "/admin/" + prof.Slug + "/wishes"
	if !isNew {
		view.Action += "/" + e.Wish.ID
	}
	assets := map[string]bool{"assets/placeholder.svg": true}
	for _, wish := range data.Snapshot.Wishes {
		if strings.HasPrefix(wish.Image, "assets/") {
			assets[wish.Image] = true
		}
	}
	if strings.HasPrefix(e.Wish.Image, "assets/") {
		assets[e.Wish.Image] = true
	}
	for path := range assets {
		view.Assets = append(view.Assets, path)
	}
	sort.Strings(view.Assets)
	return view
}

func (s *Server) adminNew(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("profile")
	if !s.authorize(w, r, p) {
		return
	}
	data, err := s.effective(r.Context(), p)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	view := s.newForm(r, data, entry{Wish: catalog.Wish{Profile: p, Image: "assets/placeholder.svg"}}, true)
	view.CSRF = ensureCSRF(w, r)
	s.execute(w, r, "admin-form.html", view)
}

func findEntry(entries []entry, id string) (entry, bool) {
	for _, e := range entries {
		if e.Wish.ID == id {
			return e, true
		}
	}
	return entry{}, false
}

func (s *Server) adminEdit(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("profile")
	if !s.authorize(w, r, p) {
		return
	}
	data, err := s.effective(r.Context(), p)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	e, ok := findEntry(data.Entries, r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	view := s.newForm(r, data, e, false)
	view.CSRF = ensureCSRF(w, r)
	s.execute(w, r, "admin-form.html", view)
}

func (s *Server) parseAdminPost(w http.ResponseWriter, r *http.Request) bool {
	if !s.authorize(w, r, r.PathValue("profile")) || !s.validPost(w, r, s.config.AdminHost, 6*1024*1024) {
		return false
	}
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var err error
	if mediaType == "multipart/form-data" {
		err = r.ParseMultipartForm(6 * 1024 * 1024)
	} else if mediaType == "application/x-www-form-urlencoded" {
		err = r.ParseForm()
	} else {
		http.Error(w, "Некорректный тип формы", http.StatusBadRequest)
		return false
	}
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			http.Error(w, "Файл слишком большой", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "Некорректная форма", http.StatusBadRequest)
		}
		return false
	}
	if len(r.PostForm) > 16 {
		http.Error(w, "Слишком много полей", http.StatusBadRequest)
		return false
	}
	for _, v := range r.PostForm {
		if len(v) != 1 {
			http.Error(w, "Повтор поля формы", http.StatusBadRequest)
			return false
		}
	}
	if !validCSRF(r) {
		http.Error(w, "Форма устарела. Обновите страницу.", http.StatusForbidden)
		return false
	}
	if p := r.PostForm.Get("profile"); p != "" && p != r.PathValue("profile") {
		http.Error(w, "Чужой профиль", http.StatusForbidden)
		return false
	}
	return true
}

func removeMultipart(r *http.Request) {
	if r.MultipartForm != nil {
		r.MultipartForm.RemoveAll()
	}
}

func (s *Server) adminSave(w http.ResponseWriter, r *http.Request) {
	defer removeMultipart(r)
	if !s.parseAdminPost(w, r) {
		return
	}
	p := r.PathValue("profile")
	data, err := s.effective(r.Context(), p)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	id := r.PathValue("id")
	isNew := id == ""
	if isNew {
		id = strings.TrimSpace(r.PostForm.Get("id"))
	}
	existing, found := findEntry(data.Entries, id)
	if !isNew && !found {
		http.NotFound(w, r)
		return
	}
	if !isNew && r.PostForm.Get("id") != "" && r.PostForm.Get("id") != id {
		http.Error(w, "ID нельзя менять", http.StatusBadRequest)
		return
	}
	revision, err := strconv.ParseInt(r.PostForm.Get("revision"), 10, 64)
	if err != nil || revision < 0 {
		http.Error(w, "Некорректная версия", http.StatusBadRequest)
		return
	}
	wish := catalog.Wish{Profile: p, ID: id, Name: strings.TrimSpace(r.PostForm.Get("name")), URL: strings.TrimSpace(r.PostForm.Get("url")), Description: strings.TrimSpace(r.PostForm.Get("description")), Note: strings.TrimSpace(r.PostForm.Get("note")), Image: strings.TrimSpace(r.PostForm.Get("image")), Category: strings.TrimSpace(r.PostForm.Get("category"))}
	if wish.Image == "" {
		if found {
			wish.Image = existing.Wish.Image
		} else {
			wish.Image = "assets/placeholder.svg"
		}
	}
	view := s.newForm(r, data, entry{Wish: wish, Revision: revision, HasGit: existing.HasGit, Hidden: existing.Hidden}, isNew)
	view.CSRF = r.PostForm.Get("csrf")
	fail := func(status int, message string) {
		view.Error = message
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		s.execute(w, r, "admin-form.html", view)
	}
	if (isNew && found) || (!isNew && existing.Revision != revision) || r.PostForm.Get("snapshot") != data.Snapshot.Revision {
		view.Snapshot = r.PostForm.Get("snapshot")
		fail(http.StatusConflict, "Список изменился. Скопируйте введённый текст и откройте форму заново, прежде чем сохранять.")
		return
	}
	media, err := readUpload(r, p)
	if err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	if media != nil {
		wish.Image = "media/" + p + "/" + media.Digest
	}
	if media == nil && strings.HasPrefix(wish.Image, "media/") {
		digest, ok := strings.CutPrefix(wish.Image, "media/"+p+"/")
		if !ok || !digestPattern.MatchString(digest) {
			fail(http.StatusBadRequest, "Изображение другого профиля")
			return
		}
		if _, e := s.database.Media(r.Context(), p, digest); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				fail(http.StatusBadRequest, "Изображение не найдено")
				return
			}
			s.internalError(w, r, e)
			return
		}
	}
	if err = catalog.Validate(data.Snapshot.Root, wish, true); err != nil {
		fail(http.StatusBadRequest, "Проверьте обязательные поля, ссылку, ID и изображение.")
		return
	}
	if media == nil {
		media, err = captureAsset(data.Snapshot.Root, wish)
		if err != nil {
			fail(http.StatusBadRequest, "Выберите JPEG, PNG или WebP либо загрузите новое изображение")
			return
		}
		if media != nil {
			wish.Image = "media/" + p + "/" + media.Digest
		}
	}

	if err = s.database.SaveOverride(r.Context(), store.Override{Wish: wish, Hidden: existing.Hidden}, revision, media); err != nil {
		if errors.Is(err, store.ErrConflict) {
			fail(http.StatusConflict, "Позиция уже изменена. Откройте форму заново.")
			return
		}
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "save", id)
	http.Redirect(w, r, "/admin/"+p, http.StatusSeeOther)
}

func (s *Server) adminAction(w http.ResponseWriter, r *http.Request, reset bool) {
	defer removeMultipart(r)
	if !s.parseAdminPost(w, r) {
		return
	}
	p, id := r.PathValue("profile"), r.PathValue("id")
	data, err := s.effective(r.Context(), p)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	e, found := findEntry(data.Entries, id)
	if !found {
		http.NotFound(w, r)
		return
	}
	revision, err := strconv.ParseInt(r.PostForm.Get("revision"), 10, 64)
	if err != nil || revision != e.Revision || r.PostForm.Get("snapshot") != data.Snapshot.Revision {
		http.Error(w, "Список изменился. Обновите страницу.", http.StatusConflict)
		return
	}
	action := "hide"
	if reset {
		if !e.HasGit || e.Revision == 0 {
			http.Error(w, "Нет версии Git для возврата", http.StatusBadRequest)
			return
		}
		err = s.database.ResetOverride(r.Context(), p, id, revision)
		action = "reset"
	} else {
		hidden := r.PostForm.Get("hidden")
		if hidden != "true" && hidden != "false" {
			http.Error(w, "Некорректное состояние", http.StatusBadRequest)
			return
		}
		media, captureErr := captureAsset(data.Snapshot.Root, e.Wish)
		if captureErr != nil {
			http.Error(w, "Не удалось сохранить изображение. Загрузите новое перед скрытием.", http.StatusBadRequest)
			return
		}
		if media != nil {
			e.Wish.Image = "media/" + p + "/" + media.Digest
		}
		err = s.database.SaveOverride(r.Context(), store.Override{Wish: e.Wish, Hidden: hidden == "true"}, revision, media)
	}
	if errors.Is(err, store.ErrConflict) {
		http.Error(w, "Позиция уже изменена. Обновите страницу.", http.StatusConflict)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, action, id)
	http.Redirect(w, r, "/admin/"+p, http.StatusSeeOther)
}

func (s *Server) adminHide(w http.ResponseWriter, r *http.Request)  { s.adminAction(w, r, false) }
func (s *Server) adminReset(w http.ResponseWriter, r *http.Request) { s.adminAction(w, r, true) }
func (s *Server) audit(r *http.Request, action, id string) {
	slog.Info("catalog changed", "user", r.Header.Get("Remote-User"), "action", action, "profile", r.PathValue("profile"), "wish_id", id)
}
