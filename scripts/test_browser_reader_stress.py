import contextlib
import importlib.util
import io
import json
import os
import pathlib
import queue
import random
import subprocess
import sys
import tempfile
import time
import unittest
import uuid
from unittest.mock import patch


sys.dont_write_bytecode = True
ROOT = pathlib.Path(__file__).parent


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, ROOT/filename)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


WORKER = load('stress_worker', 'browser-answer-worker.py')
ADAPTER = load('stress_adapter', 'browser-reader-mcp.py')
USAGE = load('stress_usage', 'browser-reader-usage.py')
FIXTURES = load('stress_fixtures', 'test_browser_reader_mcp.py')


class StressTests(unittest.TestCase):
    def test_seeded_unicode_ranges_and_budgets(self):
        rng = random.Random(2026100101)
        alphabet = 'abc XYZαé漢🙂\t\n\u0301'
        for case in range(5000):
            blocks = [''.join(rng.choices(alphabet, k=rng.randrange(1, 300))) for _ in range(rng.randrange(1, 12))]
            blocks.extend(['FACT_A.', 'FACT_B.'])
            text = '\n\n'.join(blocks)
            candidates, ranges = WORKER.passage_candidates(text, 'FACT_A FACT_B', rng.randrange(1, 300), rng.randrange(1, 32))
            budget = rng.choice([1, 2, 8, 2000, 32000, 100000, rng.randrange(1, 2000)])
            evidence, spans = WORKER.pack_passages(candidates, ranges, budget)
            self.assertLessEqual(len(evidence), budget, case)
            self.assertEqual(evidence, '\n\n'.join(text[span['start']:span['end']] for span in spans), case)
            self.assertTrue(all(0 <= span['start'] <= span['end'] <= len(text) for span in spans), case)
            self.assertTrue(all(text[ranges[key]['start']:ranges[key]['end']] == value for key, value in candidates.items()), case)

    def test_seeded_provider_frames_reject_non_objects_and_nonfinite_json(self):
        rng = random.Random(2026100102)
        values = [None, True, False, 0, [], ['text'], 'text', {}, {'choices': []}, {'usage': None}, {'usage': {'prompt_tokens': 0}}, {'usage': {'prompt_tokens': float('nan')}}]
        class Response:
            headers = {}
            def __enter__(self):
                return self
            def __exit__(self, *args):
                return None
            def read(self, size):
                return raw
        for case in range(2000):
            value = rng.choice(values)
            raw = json.dumps(value).encode()
            with patch.object(WORKER.urllib.request, 'urlopen', return_value=Response()):
                if isinstance(value, dict) and b'NaN' not in raw:
                    result, _ = WORKER.timed_http('http://127.0.0.1/fixture', {})
                    self.assertEqual(result, value)
                else:
                    with self.assertRaises(ValueError, msg=case):
                        WORKER.timed_http('http://127.0.0.1/fixture', {})

    def test_seeded_token_metadata_unknown_is_not_zero(self):
        rng = random.Random(2026100103)
        choices = [None, True, False, -1, 0, 1, 1.5, '0', {}, [], 2**1000]
        for case in range(3000):
            value = rng.choice(choices)
            row = USAGE.provider_tokens({'prompt_tokens': value, 'completion_tokens': value, 'prompt_tokens_details': {'cached_tokens': value}, 'completion_tokens_details': {'reasoning_tokens': value}})
            expected = value if type(value) is int and 0 <= value <= 9223372036854775807 else None
            for key in ('provider_input_tokens', 'provider_output_tokens', 'provider_cached_input_tokens', 'provider_reasoning_tokens'):
                self.assertEqual(row[key], expected, (case, key))

    def test_seeded_parent_metadata_cannot_crash_or_claim_outside_source_ranges(self):
        rng = random.Random(2026100105)
        values = [None, True, -1, 0, 1, 1.5, '1', float('nan'), float('inf'), 2**2048]
        for case in range(2000):
            value = rng.choice(values)
            report = {'parent_result': {'answer': 'Grounded fixture.', 'source': 'https://example.test'}, 'source_chars': value, 'worker_ms': value, 'evidence_spans': [{'start': 0, 'end': 20}]}
            packet = ADAPTER.bounded_result(report, 'fixture', 1)
            self.assertEqual(packet['answer'], 'Grounded fixture.')
            if type(value) is int and 0 <= value <= 9223372036854775807:
                self.assertEqual(packet['trace']['source_chars'], value)
                if value < 20:
                    self.assertNotIn('evidence_spans', packet['trace'])
            else:
                self.assertNotIn('source_chars', packet['trace'])

    def test_extract_mode_honors_configured_parent_character_budget(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            source = root/'source.json'
            source.write_text(json.dumps({'url': 'https://example.test', 'main': 'Short fact.\n\n'+'Background. '*1000}))
            for budget in (1, 9, 128, 1000, 2000, 5000):
                args = WORKER.parse_args(['--no-usage-log', '--source-artifact', str(source), '--out', str(root/'report.json'), '--question', 'Short fact?', '--answer-max-chars', str(budget)])
                result = WORKER.run(args)
                self.assertLessEqual(len(result['excerpt']), min(2000, budget))

    @unittest.skipUnless(os.name == 'posix', 'Owned POSIX worker groups')
    def test_inherited_stderr_does_not_outlive_worker_or_hold_capacity(self):
        with tempfile.TemporaryDirectory() as directory:
            client = FIXTURES.Client(directory, timeout=.4, concurrent=1)
            self.addCleanup(client.close)
            child = "import pathlib,sys,time; p=pathlib.Path(sys.argv[1]); (p/'child-started').write_text('1');\ntry:\n time.sleep(10)\nfinally:\n (p/'child-cleanup').write_text('1')\n"
            client.worker.write_text("import argparse,json,pathlib,subprocess,sys,time\np=argparse.ArgumentParser();p.add_argument('--url');p.add_argument('--question');p.add_argument('--out');a=p.parse_args();f=pathlib.Path(a.out)\nsubprocess.Popen([sys.executable,'-c',"+repr(child)+",str(f.parent)])\ndeadline=time.monotonic()+1\nwhile not (f.parent/'child-started').exists() and time.monotonic()<deadline: time.sleep(.005)\nf.write_text(json.dumps({'parent_result':{'answer':'fixture','source':a.url}}))\n")
            client.initialize()
            started = time.monotonic()
            client.call(request_id=2)
            self.assertEqual(client.receive(timeout=2)['id'], 2)
            self.assertLess(time.monotonic()-started, 2)
            self.assertEqual(len(list(client.artifacts.glob('*/child-cleanup'))), 1)
            client.call(request_id=3)
            self.assertFalse(client.receive(timeout=2)['result'].get('isError', False))

    def test_seeded_bursts_preserve_ids_cancelled_delivery_and_cleanup(self):
        rng = random.Random(2026100104)
        for cycle in range(12):
            with tempfile.TemporaryDirectory() as directory:
                client = FIXTURES.Client(directory, timeout=.3, concurrent=4)
                try:
                    client.initialize()
                    cancelled = set(rng.sample(range(10, 14), 2))
                    for request_id in range(10, 26):
                        client.call('sleep:.2', request_id)
                    for request_id in cancelled:
                        client.send('notifications/cancelled', {'requestId': request_id})
                    responses = [client.receive(timeout=2) for _ in range(14)]
                    ids = [response['id'] for response in responses]
                    self.assertEqual(len(ids), len(set(ids)), cycle)
                    self.assertEqual(set(ids), set(range(10, 26))-cancelled, cycle)
                    self.assertEqual(sum(bool(response['result'].get('isError')) for response in responses), 12, cycle)
                    with self.assertRaises(queue.Empty):
                        client.receive(timeout=.01)
                finally:
                    client.close()
                adapters = [json.loads(path.read_text()) for path in client.artifacts.glob('*/adapter.json')]
                self.assertEqual(len(adapters), 4, cycle)
                self.assertEqual(sum(row['cancelled'] for row in adapters), 2, cycle)

    def test_concurrent_rotation_reconfiguration_and_descriptor_accounting(self):
        with tempfile.TemporaryDirectory() as directory:
            path = str(ROOT/'browser-reader-usage.py')
            code = "import importlib.util,random,sys,uuid; s=importlib.util.spec_from_file_location('u',sys.argv[1]); u=importlib.util.module_from_spec(s); s.loader.exec_module(u); r=random.Random(int(sys.argv[3])); [(u.Ledger(sys.argv[2],max_bytes=r.choice([4096,8192,16384]),keep=r.randrange(0,4)).write('job','reader',str(uuid.uuid4()),input_bytes=i)) for i in range(500)]"
            processes = [subprocess.Popen([sys.executable, '-c', code, path, directory, str(2026100200+i)], env=dict(os.environ, PYTHONDONTWRITEBYTECODE='1'), stderr=subprocess.PIPE) for i in range(6)]
            for process in processes:
                _, error = process.communicate(timeout=30)
                self.assertEqual(process.returncode, 0, error.decode())
            USAGE.Ledger(directory, max_bytes=4096, keep=2).write('job', 'reader', str(uuid.uuid4()), input_bytes=999)
            files = list(pathlib.Path(directory).glob('reader.jsonl*'))
            self.assertLessEqual(len(files), 3)
            self.assertTrue(all(file.stat().st_size <= 4096 for file in files))
            for file in files:
                for line in file.read_text().splitlines():
                    row = json.loads(line)
                    self.assertNotIn('PRIVATE', line)
                    self.assertGreaterEqual(row['input_bytes'], 0)
            if pathlib.Path('/dev/fd').exists():
                before = len(list(pathlib.Path('/dev/fd').iterdir()))
                ledger = USAGE.Ledger(directory, max_bytes=4096, keep=2)
                for index in range(1000):
                    ledger.write('job', 'reader', str(uuid.uuid4()), input_bytes=index)
                after = len(list(pathlib.Path('/dev/fd').iterdir()))
                self.assertLessEqual(after, before+1)

    def test_http_error_body_failure_closes_provider_descriptor(self):
        from urllib.error import HTTPError
        class Body(io.BytesIO):
            def read(self, *args):
                raise OSError('fixture read interruption')
        body = Body(b'fixture')
        error = HTTPError('http://127.0.0.1/fixture', 500, 'fixture', {}, body)
        with patch.object(WORKER.urllib.request, 'urlopen', side_effect=error):
            with self.assertRaises(OSError):
                WORKER.timed_http('http://127.0.0.1/fixture', {})
        self.assertTrue(body.closed)


if __name__ == '__main__':
    unittest.main()
