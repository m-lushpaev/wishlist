#!/usr/bin/python3
import argparse
from pathlib import Path
import re


FILTER = '''format filter {
            wrap json
            fields {
                request>uri delete
                request>headers>Referer delete
                resp_headers>Location delete
            }
        }'''
ERROR_LOG = '''
    log wishlist_errors {
        include http.log.error.wishlist_public http.log.error.wishlist_admin
        output stdout
        %s
    }
''' % FILTER


def caddy_config(text):
    for host, name in [('wish.lushpaev.ru', 'wishlist_public'),
                       ('wish-admin.lushpaev.ru', 'wishlist_admin')]:
        start = text.index(host + ' {')
        opening = text.index('{', start)
        depth = 0
        end = None
        for index in range(opening, len(text)):
            depth += (text[index] == '{') - (text[index] == '}')
            if depth == 0:
                end = index + 1
                break
        if end is None:
            raise ValueError('unbalanced site block')
        block = text[start:end]
        block = block.replace('Referrer-Policy strict-origin-when-cross-origin', 'Referrer-Policy no-referrer')
        if 'log ' + name + ' {' not in block:
            if block.count('log {') != 1 or block.count('format json') != 1:
                raise ValueError('unexpected wishlist logging configuration')
            block = block.replace('log {', 'log ' + name + ' {').replace('format json', FILTER)
        text = text[:start] + block + text[end:]
    if 'log wishlist_errors {' not in text:
        stripped = text.lstrip()
        if not stripped.startswith('{'):
            raise ValueError('expected existing Caddy global options')
        opening = text.index('{')
        text = text[:opening + 1] + ERROR_LOG + text[opening + 1:]
    return text


def authelia_config(text):
    import yaml
    groups = ['group:admins'] + ['group:wishlist-' + p for p in ('maksim', 'anya', 'grisha', 'arisha')]
    original = yaml.safe_load(text)
    rules = original['access_control']['rules']
    for rule in rules:
        domains = rule.get('domain', [])
        if domains == 'wish-admin.lushpaev.ru' or domains == ['wish-admin.lushpaev.ru']:
            if rule.get('subject') == groups and rule.get('policy') == 'one_factor':
                return text
            raise ValueError('unexpected dedicated wishlist ACL')
    if not any('wish-admin.lushpaev.ru' in rule.get('domain', []) for rule in rules):
        raise ValueError('existing wishlist ACL not found')
    changed = insert_acl_rule(text, groups)
    parsed = yaml.safe_load(changed)
    if parsed['access_control']['rules'][1:] != rules:
        raise ValueError('unexpected ACL changes')
    return changed


def insert_acl_rule(text, groups):
    section = re.search(r'^access_control:\s*\n(?:(?!^[^\s#]).*\n)*', text, re.M)
    if not section:
        raise ValueError('access_control section not found')
    part = section.group()
    anchor = re.search(r'^( +)rules:[ \t]*\n', part, re.M)
    if not anchor:
        raise ValueError('rules anchor not found')
    sequence = re.search(r'^( *)-[ \t]', part[anchor.end():], re.M)
    if not sequence or len(sequence.group(1)) < len(anchor.group(1)):
        raise ValueError('rules sequence indentation not found')
    indent = sequence.group(1)
    addition = indent + '- domain: wish-admin.lushpaev.ru\n' + indent + '  subject:\n'
    addition += ''.join(indent + '    - ' + group + '\n' for group in groups)
    addition += indent + '  policy: one_factor\n'
    offset = section.start() + anchor.end()
    return text[:offset] + addition + text[offset:]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('kind', choices=['caddy', 'authelia'])
    parser.add_argument('source', type=Path)
    parser.add_argument('destination', type=Path)
    args = parser.parse_args()
    source = args.source.read_text()
    result = caddy_config(source) if args.kind == 'caddy' else authelia_config(source)
    args.destination.write_text(result)
    args.destination.chmod(0o600)
    print(args.kind + ' configuration prepared')


if __name__ == '__main__':
    main()
