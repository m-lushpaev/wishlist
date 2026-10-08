#!/usr/bin/python3
"""Create a consistent SQLite backup and keep the latest 14 daily copies."""
from datetime import datetime, timezone
from pathlib import Path
import os
import sqlite3

SOURCE = Path('/var/lib/wishlist/app/wishlist.db')
DESTINATION = Path('/var/backups/wishlist')

DESTINATION.mkdir(mode=0o700, parents=True, exist_ok=True)
target = DESTINATION / ('wishlist-' + datetime.now(timezone.utc).strftime('%Y%m%d') + '.sqlite')
temporary = target.with_suffix('.tmp')
temporary.unlink(missing_ok=True)
with sqlite3.connect(f'file:{SOURCE}?mode=ro', uri=True) as source:
    with sqlite3.connect(temporary) as destination:
        source.backup(destination)
os.chmod(temporary, 0o600)
temporary.replace(target)
for old in sorted(DESTINATION.glob('wishlist-*.sqlite'), reverse=True)[14:]:
    old.unlink()
print('Wishlist backup created: ' + target.name)
