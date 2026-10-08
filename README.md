# Wishlist

Me want things. Me put things here.

[See list](https://wish.lushpaev.ru/).

Go serve page. SQLite remember reservations. Little HTMX. No trackers.

Friend reserve gift. Friend get secret undo link. Public page show no friend name.

## Change things

Edit [data/wishes.ini](data/wishes.ini). One `[wish]` block, one thing.
Add stable id, name, link, description, photo, category. Note can be empty.
Put photos in `site/assets/`. Save to main. Robot check code. Server pull words and photos.

Change look: [site/styles.css](site/styles.css).
Change page: [internal/server/templates](internal/server/templates/).

## Build here

Need Go and make.

```sh
make
```

Run `dist/wishlist`. Done.
