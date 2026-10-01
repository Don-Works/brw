import contextlib
import datetime
import math
import os
import pathlib
import stat
import sys
import time
import json
import uuid


def default_directory():
    if sys.platform == 'darwin':
        root = pathlib.Path.home() / 'Library/Application Support'
    elif os.name == 'nt':
        root = pathlib.Path(os.environ.get('APPDATA') or pathlib.Path.home() / 'AppData/Roaming')
    else:
        root = pathlib.Path(os.environ.get('XDG_CONFIG_HOME') or pathlib.Path.home() / '.config')
    return str(root / 'brw/usage')


def timestamp():
    return datetime.datetime.now(datetime.timezone.utc).isoformat().replace('+00:00', 'Z')


def token_count(value):
    return value if isinstance(value, int) and not isinstance(value, bool) and 0 <= value <= 9223372036854775807 else None


def provider_tokens(usage):
    usage = usage if isinstance(usage, dict) else {}
    prompt = usage.get('prompt_tokens_details') if isinstance(usage.get('prompt_tokens_details'), dict) else {}
    completion = usage.get('completion_tokens_details') if isinstance(usage.get('completion_tokens_details'), dict) else {}
    return {
        'provider_input_tokens': token_count(usage.get('prompt_tokens', usage.get('input_tokens'))),
        'provider_output_tokens': token_count(usage.get('completion_tokens', usage.get('output_tokens'))),
        'provider_cached_input_tokens': token_count(prompt.get('cached_tokens', usage.get('cache_read_input_tokens'))),
        'provider_cache_write_tokens': token_count(usage.get('cache_creation_input_tokens')),
        'provider_reasoning_tokens': token_count(completion.get('reasoning_tokens')),
    }


class Ledger:
    def __init__(self, directory=None, enabled=True, max_bytes=1048576, keep=3):
        self.directory = pathlib.Path(directory or default_directory()).expanduser()
        self.enabled = enabled and os.environ.get('BRW_READER_USAGE_ENABLED') != '0'
        self.max_bytes = max_bytes
        self.keep = keep
        self.warned = False

    def private_open(self, path):
        fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_APPEND | getattr(os, 'O_NOFOLLOW', 0), 0o600)
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or (os.name == 'posix' and info.st_mode & 0o077):
            os.close(fd)
            raise OSError('Usage file is not private')
        return os.fdopen(fd, 'a+b')

    @contextlib.contextmanager
    def locked(self):
        if self.directory.is_symlink():
            raise OSError('Usage directory is a symlink')
        self.directory.mkdir(parents=True, mode=0o700, exist_ok=True)
        if os.name == 'posix' and self.directory.stat().st_mode & 0o077:
            raise OSError('Usage directory is not private')
        with self.private_open(self.directory / '.reader.lock') as lock:
            if os.name == 'posix':
                import fcntl
                deadline = time.monotonic()+.2
                while True:
                    try:
                        fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                        break
                    except BlockingIOError:
                        if time.monotonic() >= deadline:
                            raise OSError('Usage lock timeout') from None
                        time.sleep(.01)
            elif os.name == 'nt':
                import msvcrt
                if lock.seek(0, 2) == 0:
                    lock.write(b'0')
                    lock.flush()
                lock.seek(0)
                deadline = time.monotonic()+.2
                while True:
                    try:
                        msvcrt.locking(lock.fileno(), msvcrt.LK_NBLCK, 1)
                        break
                    except OSError:
                        if time.monotonic() >= deadline:
                            raise OSError('Usage lock timeout') from None
                        time.sleep(.01)
            else:
                raise OSError('Usage locking is unavailable')
            try:
                yield
            finally:
                if os.name == 'posix':
                    fcntl.flock(lock.fileno(), fcntl.LOCK_UN)
                else:
                    lock.seek(0)
                    msvcrt.locking(lock.fileno(), msvcrt.LK_UNLCK, 1)

    def write(self, operation, scope, trace_id, **fields):
        if not self.enabled:
            return
        operations = {'job', 'adapter', 'answer', 'classifier'}
        scopes = {'reader', 'model', 'transport'}
        allowed = {'started_at', 'finished_at', 'duration_ms', 'duration_us', 'input_bytes', 'output_bytes', 'input_text_chars', 'output_text_chars', 'estimated_input_tokens_chars4', 'estimated_output_tokens_chars4', 'provider_input_tokens', 'provider_output_tokens', 'provider_cached_input_tokens', 'provider_cache_write_tokens', 'provider_reasoning_tokens', 'mode', 'representation', 'outcome', 'collection_ms', 'worker_ms', 'request_to_headers_ms', 'first_visible_delta_ms', 'source_chars', 'source_total_chars', 'source_truncated', 'evidence_chars', 'evidence_narrowed', 'cleanup_ms', 'cancelled', 'job_id', 'request_id', 'serialization_ms', 'response_decode_ms', 'collection_health_ms', 'collection_open_ms', 'collection_read_ms', 'collection_cleanup_ms', 'collection_replayed'}
        try:
            trace_id = str(uuid.UUID(trace_id))
            if operation not in operations or scope not in scopes:
                raise ValueError('Invalid usage operation')
            row = {'schema_version': 1, 'ts': timestamp(), 'layer': 'reader', 'operation': operation, 'scope': scope, 'trace_id': trace_id}
            for key, value in fields.items():
                if key not in allowed:
                    continue
                if value is None or isinstance(value, bool) or (isinstance(value, int) and -9223372036854775808 <= value <= 9223372036854775807) or (isinstance(value, float) and math.isfinite(value)):
                    row[key] = value
                elif key in {'started_at', 'finished_at'}:
                    datetime.datetime.fromisoformat(value.replace('Z', '+00:00'))
                    row[key] = value
                elif key in {'job_id', 'request_id'}:
                    row[key] = str(uuid.UUID(value))
                elif key == 'mode' and value in {'off', 'shadow', 'select'}:
                    row[key] = value
                elif key == 'outcome' and value in {'success', 'error', 'cancelled', 'deadline_exceeded'}:
                    row[key] = value
                elif key == 'representation' and value in {'json', 'jsonrpc', 'utf8', 'arguments_json'}:
                    row[key] = value
            raw = (json.dumps(row, allow_nan=False, separators=(',', ':')) + '\n').encode()
            if len(raw) > self.max_bytes:
                raise ValueError('Usage event exceeds rotation size')
            with self.locked():
                path = self.directory / 'reader.jsonl'
                paths = [path] + [self.directory / ('reader.jsonl.' + str(i)) for i in range(1, self.keep + 1)]
                if any(p.is_symlink() for p in paths):
                    raise OSError('Usage file is a symlink')
                if path.exists() and path.stat().st_size + len(raw) > self.max_bytes:
                    if self.keep:
                        for index in range(self.keep, 1, -1):
                            if paths[index-1].exists():
                                os.replace(paths[index-1], paths[index])
                        os.replace(path, paths[1])
                    else:
                        path.unlink()
                for archive in self.directory.glob('reader.jsonl.*'):
                    suffix = archive.name[len('reader.jsonl.'):]
                    if not suffix.isdigit():
                        continue
                    if archive.is_symlink():
                        raise OSError('Usage file is a symlink')
                    if int(suffix) > self.keep or archive.stat().st_size > self.max_bytes:
                        archive.unlink()
                with self.private_open(path) as output:
                    output.write(raw)
                    output.flush()
        except (OSError, ValueError, TypeError):
            if not self.warned:
                print('Reader usage logging unavailable; reader operation continues.', file=sys.stderr)
                self.warned = True


def from_args(args):
    if os.environ.get('BRW_READER_USAGE_PARENT') == '1':
        return Ledger(os.environ['BRW_READER_USAGE_DIR'], args.usage_log, int(os.environ['BRW_READER_USAGE_MAX_BYTES']), int(os.environ['BRW_READER_USAGE_KEEP']))
    return Ledger(args.usage_dir, args.usage_log, args.usage_max_bytes, args.usage_keep)


def add_arguments(parser):
    parser.add_argument('--usage-dir', default=os.environ.get('BRW_READER_USAGE_DIR') or default_directory())
    parser.add_argument('--no-usage-log', dest='usage_log', action='store_false', default=True)
    parser.add_argument('--usage-max-bytes', type=int, default=1048576)
    parser.add_argument('--usage-keep', type=int, default=3)


def validate(args, parser):
    if not isinstance(args.usage_log, bool):
        parser.error('usage_log must be a boolean')
    if not isinstance(args.usage_dir, str) or not args.usage_dir:
        parser.error('usage_dir must be a nonempty path')
    if isinstance(args.usage_max_bytes, bool) or not isinstance(args.usage_max_bytes, int) or not 4096 <= args.usage_max_bytes <= 67108864:
        parser.error('usage_max_bytes must be between 4096 and 67108864')
    if isinstance(args.usage_keep, bool) or not isinstance(args.usage_keep, int) or not 0 <= args.usage_keep <= 16:
        parser.error('usage_keep must be between 0 and 16')
