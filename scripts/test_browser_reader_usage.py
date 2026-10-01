import contextlib
import importlib.util
import io
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import time
import unittest
import uuid
from unittest.mock import patch


sys.dont_write_bytecode = True
PATH = pathlib.Path(__file__).with_name('browser-reader-usage.py')
SPEC = importlib.util.spec_from_file_location('usage', PATH)
USAGE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(USAGE)


class UsageTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = pathlib.Path(self.directory.name)

    def test_rotation_is_private_and_bounded(self):
        ledger = USAGE.Ledger(self.root/'usage', max_bytes=4096, keep=2)
        for index in range(100):
            ledger.write('job', 'reader', str(uuid.uuid4()), input_bytes=index, output_bytes=10, prompt='PRIVATE', url='https://private.test')
        files = list((self.root/'usage').glob('reader.jsonl*'))
        self.assertEqual(len(files), 3)
        for file in files:
            self.assertLessEqual(file.stat().st_size, 4096)
            if os.name == 'posix':
                self.assertEqual(file.stat().st_mode & 0o077, 0)
            for line in file.read_text().splitlines():
                row = json.loads(line)
                self.assertNotIn('PRIVATE', line)
                self.assertNotIn('private', line)
                self.assertNotIn('prompt', row)
                self.assertNotIn('url', row)
        self.assertEqual(json.loads((self.root/'usage/reader.jsonl').read_text().splitlines()[-1])['input_bytes'], 99)

    def test_lower_retention_limits_remove_old_oversized_archives(self):
        directory = self.root/'usage'
        ledger = USAGE.Ledger(directory, max_bytes=8192, keep=4)
        for index in range(250):
            ledger.write('job', 'reader', str(uuid.uuid4()), input_bytes=index)
        self.assertTrue((directory/'reader.jsonl.4').exists())
        USAGE.Ledger(directory, max_bytes=4096, keep=1).write('job', 'reader', str(uuid.uuid4()), input_bytes=999)
        files = list(directory.glob('reader.jsonl*'))
        self.assertLessEqual(len(files), 2)
        self.assertTrue(all(file.stat().st_size <= 4096 for file in files))
        self.assertFalse((directory/'reader.jsonl.4').exists())
        self.assertEqual(json.loads((directory/'reader.jsonl').read_text().splitlines()[-1])['input_bytes'], 999)
        USAGE.Ledger(directory, max_bytes=4096, keep=0).write('job', 'reader', str(uuid.uuid4()))
        self.assertEqual(len(list(directory.glob('reader.jsonl*'))), 1)

    def test_concurrent_processes_share_complete_records(self):
        code = "import importlib.util,sys,uuid; s=importlib.util.spec_from_file_location('u',sys.argv[1]); u=importlib.util.module_from_spec(s); s.loader.exec_module(u); l=u.Ledger(sys.argv[2],max_bytes=65536); [l.write('job','reader',str(uuid.uuid4()),input_bytes=i) for i in range(20)]"
        environment = dict(os.environ, PYTHONDONTWRITEBYTECODE='1')
        processes = [subprocess.Popen([sys.executable, '-c', code, str(PATH), str(self.root/'usage')], env=environment) for _ in range(6)]
        for process in processes:
            self.assertEqual(process.wait(timeout=10), 0)
        rows = [json.loads(line) for file in (self.root/'usage').glob('reader.jsonl*') for line in file.read_text().splitlines()]
        self.assertEqual(len(rows), 120)
        self.assertEqual(len({row['trace_id'] for row in rows}), 120)

    @unittest.skipUnless(os.name == 'posix', 'POSIX private file controls')
    def test_symlinks_and_public_paths_fail_without_following(self):
        target = self.root/'target'
        target.write_text('original')
        directory = self.root/'usage'
        directory.mkdir(mode=0o700)
        (directory/'reader.jsonl').symlink_to(target)
        with contextlib.redirect_stderr(io.StringIO()):
            USAGE.Ledger(directory).write('job', 'reader', str(uuid.uuid4()))
        self.assertEqual(target.read_text(), 'original')
        public = self.root/'public'
        public.mkdir(mode=0o755)
        with contextlib.redirect_stderr(io.StringIO()):
            USAGE.Ledger(public).write('job', 'reader', str(uuid.uuid4()))
        self.assertFalse((public/'reader.jsonl').exists())

    @unittest.skipUnless(os.name == 'posix', 'POSIX locking')
    def test_contended_usage_lock_does_not_stall_reader(self):
        import fcntl
        ledger = USAGE.Ledger(self.root/'usage')
        ledger.directory.mkdir(mode=0o700)
        with ledger.private_open(ledger.directory/'.reader.lock') as lock:
            fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
            started = time.monotonic()
            with contextlib.redirect_stderr(io.StringIO()):
                ledger.write('job', 'reader', str(uuid.uuid4()))
            self.assertLess(time.monotonic()-started, 1)
        self.assertFalse((ledger.directory/'reader.jsonl').exists())

    def test_unknown_tokens_stay_unknown_and_zero_stays_zero(self):
        empty = USAGE.provider_tokens(None)
        self.assertTrue(all(value is None for value in empty.values()))
        values = USAGE.provider_tokens({'prompt_tokens': True, 'completion_tokens': -1, 'prompt_tokens_details': {'cached_tokens': 0}, 'completion_tokens_details': 'invalid'})
        self.assertIsNone(values['provider_input_tokens'])
        self.assertIsNone(values['provider_output_tokens'])
        self.assertEqual(values['provider_cached_input_tokens'], 0)
        self.assertIsNone(values['provider_reasoning_tokens'])

    def test_parent_rotation_limits_override_worker_configuration(self):
        import argparse
        args = argparse.Namespace(usage_dir='unused', usage_log=True, usage_max_bytes=1048576, usage_keep=3)
        values = {'BRW_READER_USAGE_PARENT': '1', 'BRW_READER_USAGE_DIR': str(self.root/'usage'), 'BRW_READER_USAGE_MAX_BYTES': '4096', 'BRW_READER_USAGE_KEEP': '1'}
        with patch.dict(os.environ, values):
            ledger = USAGE.from_args(args)
        self.assertEqual(ledger.directory, self.root/'usage')
        self.assertEqual(ledger.max_bytes, 4096)
        self.assertEqual(ledger.keep, 1)

    def test_platform_defaults_and_environment_opt_out(self):
        with patch.object(USAGE.sys, 'platform', 'darwin'), patch.object(USAGE.pathlib.Path, 'home', return_value=pathlib.Path('/test-root/operator')):
            self.assertEqual(USAGE.default_directory(), '/test-root/operator/Library/Application Support/brw/usage')
        with patch.object(USAGE.sys, 'platform', 'linux'), patch.object(USAGE.pathlib.Path, 'home', return_value=pathlib.Path('/test-root/operator')), patch.dict(os.environ, {'XDG_CONFIG_HOME': ''}):
            self.assertEqual(USAGE.default_directory(), '/test-root/operator/.config/brw/usage')
        with patch.object(USAGE.sys, 'platform', 'linux'), patch.dict(os.environ, {'XDG_CONFIG_HOME': '/tmp/operator-config'}):
            self.assertEqual(USAGE.default_directory(), '/tmp/operator-config/brw/usage')
        with patch.dict(os.environ, {'BRW_READER_USAGE_ENABLED': '0'}):
            USAGE.Ledger(self.root/'disabled').write('job', 'reader', str(uuid.uuid4()))
        self.assertFalse((self.root/'disabled').exists())


if __name__ == '__main__':
    unittest.main()
