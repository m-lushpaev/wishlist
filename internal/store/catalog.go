package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/m-lushpaev/wishlist/internal/catalog"
)

type Override struct {
	Wish     catalog.Wish
	Hidden   bool
	Revision int64
}

type Media struct {
	Profile, Digest, MIME string
	Body                  []byte
}

func (d *DB) ListOverrides(ctx context.Context, p string) (map[string]Override, error) {
	if err := validProfile(p); err != nil {
		return nil, err
	}
	rows, err := d.db.QueryContext(ctx, `SELECT wish_id,name,url,description,note,image,category,hidden,revision FROM catalog_overrides WHERE profile=? ORDER BY updated_at,wish_id`, p)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]Override{}
	for rows.Next() {
		o := Override{Wish: catalog.Wish{Profile: p}}
		if err = rows.Scan(&o.Wish.ID, &o.Wish.Name, &o.Wish.URL, &o.Wish.Description, &o.Wish.Note, &o.Wish.Image, &o.Wish.Category, &o.Hidden, &o.Revision); err != nil {
			return nil, err
		}
		result[o.Wish.ID] = o
	}
	return result, rows.Err()
}

func (d *DB) SaveOverride(ctx context.Context, o Override, expected int64, media *Media) error {
	w := o.Wish
	if err := validProfile(w.Profile); err != nil {
		return err
	}
	if expected < 0 {
		return ErrConflict
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if media != nil {
		if media.Profile != w.Profile || w.Image != "media/"+w.Profile+"/"+media.Digest {
			return errors.New("invalid media profile")
		}
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO media(profile,digest,mime,body) VALUES(?,?,?,?)", media.Profile, media.Digest, media.MIME, media.Body); err != nil {
			return err
		}
	}
	if strings.HasPrefix(w.Image, "media/") {
		digest, ok := strings.CutPrefix(w.Image, "media/"+w.Profile+"/")
		if !ok || len(digest) != 64 {
			return errors.New("invalid media reference")
		}
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM media WHERE profile=? AND digest=?", w.Profile, digest).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return errors.New("missing media")
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE catalog_clock SET version=version+1 WHERE id=1"); err != nil {
		return err
	}
	var nextRevision int64
	if err = tx.QueryRowContext(ctx, "SELECT version FROM catalog_clock WHERE id=1").Scan(&nextRevision); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if expected == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO catalog_overrides(profile,wish_id,name,url,description,note,image,category,hidden,revision,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, w.Profile, w.ID, w.Name, w.URL, w.Description, w.Note, w.Image, w.Category, o.Hidden, nextRevision, now)
		if err != nil {
			if isUnique(err) {
				return ErrConflict
			}
			return err
		}
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE catalog_overrides SET name=?,url=?,description=?,note=?,image=?,category=?,hidden=?,revision=?,updated_at=? WHERE profile=? AND wish_id=? AND revision=?`, w.Name, w.URL, w.Description, w.Note, w.Image, w.Category, o.Hidden, nextRevision, now, w.Profile, w.ID, expected)
		if err != nil {
			return err
		}
		n, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrConflict
		}
	}
	return tx.Commit()
}

func (d *DB) ResetOverride(ctx context.Context, p, id string, expected int64) error {
	if err := validProfile(p); err != nil {
		return err
	}
	result, err := d.db.ExecContext(ctx, "DELETE FROM catalog_overrides WHERE profile=? AND wish_id=? AND revision=?", p, id, expected)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}

func (d *DB) Media(ctx context.Context, p, digest string) (Media, error) {
	if err := validProfile(p); err != nil {
		return Media{}, err
	}
	m := Media{Profile: p, Digest: digest}
	err := d.db.QueryRowContext(ctx, "SELECT mime,body FROM media WHERE profile=? AND digest=?", p, digest).Scan(&m.MIME, &m.Body)
	return m, err
}

func isUnique(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}
