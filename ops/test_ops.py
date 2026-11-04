import importlib.util
import io
from pathlib import Path
import sqlite3
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch


def load(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class OpsTests(unittest.TestCase):
    def test_backup_preserves_all_content_and_is_private(self):
        module = load('backup')
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            module.SOURCE = root / 'source.sqlite'
            module.DESTINATION = root / 'backups'
            with sqlite3.connect(module.SOURCE) as db:
                db.executescript('CREATE TABLE reservations(id TEXT); CREATE TABLE media(body BLOB);')
                db.execute('INSERT INTO reservations VALUES(?)', ('keep',))
                db.execute('INSERT INTO media VALUES(?)', (b'image',))
            module.backup()
            copies = list(module.DESTINATION.glob('*.sqlite'))
            self.assertEqual(len(copies), 1)
            self.assertEqual(copies[0].stat().st_mode & 0o777, 0o600)
            with sqlite3.connect(copies[0]) as db:
                self.assertEqual(db.execute('PRAGMA integrity_check').fetchone()[0], 'ok')
                self.assertEqual(db.execute('SELECT id FROM reservations').fetchone()[0], 'keep')
                self.assertEqual(db.execute('SELECT body FROM media').fetchone()[0], b'image')

    def test_invalid_release_does_not_switch_current(self):
        module = load('update')
        with tempfile.TemporaryDirectory() as folder:
            module.ROOT = Path(folder)
            releases = module.ROOT / 'releases'
            releases.mkdir()
            previous = releases / 'previous'
            previous.mkdir()
            (module.ROOT / 'current').symlink_to(previous)
            archive = io.BytesIO()
            with tarfile.open(fileobj=archive, mode='w') as tar:
                for name, content in [('data/wishes.ini', b'invalid'), ('site/styles.css', b'body{}')]:
                    entry = tarfile.TarInfo(name)
                    entry.size = len(content)
                    tar.addfile(entry, io.BytesIO(content))
            def git(*args):
                return b'next' if args[0] == 'rev-parse' else archive.getvalue()
            with patch.object(module, 'fetch_main'), patch.object(module, 'git', git), patch.object(module.subprocess, 'run', side_effect=subprocess.CalledProcessError(1, 'check-content')):
                with self.assertRaises(subprocess.CalledProcessError):
                    module.deploy()
            self.assertEqual((module.ROOT / 'current').resolve(), previous.resolve())
            self.assertFalse((releases / 'next').exists())
            self.assertFalse(list(module.ROOT.glob('.staging-*')))

    def test_archive_cannot_escape_or_use_symlinks(self):
        module = load('update')
        for unsafe in ('../escape', 'site/assets/link'):
            with self.subTest(unsafe=unsafe), tempfile.TemporaryDirectory() as folder:
                module.ROOT = Path(folder)
                (module.ROOT / 'releases').mkdir()
                data = io.BytesIO()
                with tarfile.open(fileobj=data, mode='w') as tar:
                    item = tarfile.TarInfo(unsafe)
                    if unsafe.endswith('link'):
                        item.type = tarfile.SYMTYPE
                        item.linkname = '/etc/passwd'
                    tar.addfile(item)
                with patch.object(module, 'fetch_main'), patch.object(module, 'git', side_effect=[b'next', data.getvalue()]):
                    with self.assertRaises(AssertionError):
                        module.deploy()
                self.assertFalse((module.ROOT / 'current').exists())

    def test_caddy_change_is_targeted_and_idempotent(self):
        module = load('family_config')
        source = '{\n admin off\n}\n'
        for host in ('wish.lushpaev.ru', 'wish-admin.lushpaev.ru'):
            source += host + ' {\n Referrer-Policy strict-origin-when-cross-origin\n log {\n output stdout\n format json\n }\n}\n'
        other = 'other.example {\n respond 200\n}\n'
        result = module.caddy_config(source + other)
        self.assertIn(other, result)
        self.assertEqual(result, module.caddy_config(result))
        self.assertIn('http.log.error.wishlist_public', result)
        self.assertEqual(result.count('request>uri delete'), 3)
        self.assertNotIn('strict-origin-when-cross-origin', result)


    def test_authelia_preserves_indentless_rules(self):
        module = load('family_config')
        source = 'access_control:\n  default_policy: deny\n  rules:\n  - domain: old.example\n    policy: one_factor\n'
        result = module.insert_acl_rule(source, ['group:admins'])
        expected = 'access_control:\n  default_policy: deny\n  rules:\n  - domain: wish-admin.lushpaev.ru\n    subject:\n      - group:admins\n    policy: one_factor\n  - domain: old.example\n    policy: one_factor\n'
        self.assertEqual(result, expected)
        indented = source.replace('  - domain:', '    - domain:').replace('    policy:', '      policy:')
        result = module.insert_acl_rule(indented, ['group:admins'])
        self.assertIn('    - domain: wish-admin.lushpaev.ru\n      subject:\n        - group:admins', result)
