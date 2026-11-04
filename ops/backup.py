#!/usr/bin/python3
from datetime import datetime, timezone
from pathlib import Path
import os
import sqlite3

SOURCE = Path('/var/lib/wishlist/app/wishlist.db')
DESTINATION = Path('/var/backups/wishlist')

def backup():
    DESTINATION.mkdir(mode=0o700, parents=True, exist_ok=True)
    target = DESTINATION / ('wishlist-' + datetime.now(timezone.utc).strftime('%Y%m%d') + '.sqlite')
    temporary = target.with_suffix('.tmp')
    temporary.unlink(missing_ok=True)
    descriptor = os.open(temporary, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    os.close(descriptor)
    try:
        with sqlite3.connect(f'file:{SOURCE}?mode=ro', uri=True) as source:
            with sqlite3.connect(temporary) as destination:
                source.backup(destination)
                if destination.execute('PRAGMA integrity_check').fetchone()[0] != 'ok':
                    raise RuntimeError('backup integrity check failed')
        temporary.replace(target)
        for old in sorted(DESTINATION.glob('wishlist-*.sqlite'), reverse=True)[14:]:
            old.unlink()
        print('Wishlist backup created: ' + target.name)
    finally:
        temporary.unlink(missing_ok=True)

if __name__ == '__main__':
    os.umask(0o077)
    backup()
