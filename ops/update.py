#!/usr/bin/python3
"""Deploy only static files from a public Git repository, atomically."""
import io
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile

ROOT = Path('/srv/wishlist')
REPO = Path('/var/lib/wishlist/repo.git')
URL = 'https://github.com/m-lushpaev/wishlist.git'

def git(*args):
    return subprocess.run(['git', '--git-dir', str(REPO), *args], check=True,
                          capture_output=True, timeout=45).stdout

def init_repo():
    if not REPO.exists():
        subprocess.run(['git', 'init', '--bare', str(REPO)], check=True,
                       capture_output=True, timeout=15)

def fetch_main():
    init_repo()
    try:
        git('fetch', '--depth=1', URL, 'refs/heads/main')
    except subprocess.CalledProcessError:
        # The cache is disposable. Recreate it after an interrupted fetch leaves
        # a lock file or otherwise makes the shallow repository unusable.
        shutil.rmtree(REPO)
        init_repo()
        git('fetch', '--depth=1', URL, 'refs/heads/main')

def deploy():
    fetch_main()
    revision = git('rev-parse', 'FETCH_HEAD').decode().strip()
    destination = ROOT / 'releases' / revision
    current = ROOT / 'current'
    if current.is_symlink() and current.resolve() == destination:
        print('Wishlist unchanged: ' + revision[:12])
        return
    archive = git('archive', '--format=tar', revision,
                  'data/wishes.ini', 'site/assets', 'site/styles.css')
    assert len(archive) <= 20 * 1024 * 1024, 'Static site exceeds 20 MiB'
    staging = Path(tempfile.mkdtemp(prefix='.staging-', dir=ROOT))
    try:
        with tarfile.open(fileobj=io.BytesIO(archive)) as tar:
            for member in tar.getmembers():
                path = Path(member.name)
                assert not path.is_absolute() and '..' not in path.parts, 'Invalid archive path'
                assert member.isfile() or member.isdir(), 'Links and special files are forbidden'
                assert member.size <= 5 * 1024 * 1024, 'File exceeds 5 MiB'
            tar.extractall(staging, filter='data')
        for name in ('data/wishes.ini', 'site/styles.css'):
            assert (staging/name).is_file(), 'Missing required site file: '+name
        for file in staging.rglob('*'):
            file.chmod(0o755 if file.is_dir() else 0o644)
        staging.chmod(0o755)
        if not destination.exists():
            staging.rename(destination)
        link = ROOT / '.current-next'
        link.unlink(missing_ok=True)
        link.symlink_to(destination)
        link.replace(current)
        # Keep current and the preceding revision for rollback.
        releases = sorted((ROOT/'releases').iterdir(),key=lambda p:p.stat().st_mtime,reverse=True)
        keep = {destination}
        keep.update(p for p in releases if p != destination and len(keep) < 2)
        for old in releases:
            if old not in keep:
                shutil.rmtree(old)
        print('Wishlist deployed: ' + revision[:12])
    finally:
        if staging.exists():
            shutil.rmtree(staging)

if __name__ == '__main__':
    os.umask(0o022)
    try:
        deploy()
    except (AssertionError, OSError, ValueError, subprocess.SubprocessError) as error:
        # Never emit raw git output or remote file contents.
        print('Wishlist deploy failed: ' + type(error).__name__)
        raise SystemExit(1)
