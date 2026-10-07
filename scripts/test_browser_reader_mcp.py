import importlib.util
import io
import json
import os
import pathlib
import queue
import subprocess
import sys
import tempfile
from unittest.mock import Mock, patch
import threading
import time
import unittest


PATH = pathlib.Path(__file__).with_name('browser-reader-mcp.py')
SPEC = importlib.util.spec_from_file_location('browser_reader_mcp', PATH)
ADAPTER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ADAPTER)
WORKER = '''import argparse
import json
import pathlib
import sys
import time
p = argparse.ArgumentParser()
p.add_argument('--config')
p.add_argument('--url')
p.add_argument('--question')
p.add_argument('--out')
a = p.parse_args()
f = pathlib.Path(a.out)
(f.parent / 'started').write_text('yes')
(f.parent / 'config-argument').write_text(a.config or '')
if a.question.startswith('sleep:'):
    try:
        time.sleep(float(a.question.split(':')[1]))
    finally:
        (f.parent / 'cleanup').write_text('yes')
if a.question == 'stderr-flood':
    print('x' * 100000, file=sys.stderr)
if a.question == 'failure':
    f.write_text(json.dumps({'error': 'PRIVATE_API_KEY'}))
    print('PRIVATE_API_KEY', file=sys.stderr)
    raise SystemExit(1)
if a.question == 'oversized-report':
    f.write_text('x' * 1048577)
    raise SystemExit(0)
answer = 'Grounded answer.' if a.question != 'oversized-answer' else 'x' * 2001
report = {'parent_result': {'answer': answer, 'source': a.url, 'secret': 'PRIVATE_API_KEY'}, 'worker_ms': 3.0, 'collection_ms': 1.0, 'source_chars': 40000, 'evidence_chars': 2000, 'mode': {'answer_model': 'model', 'classifier': 'select'}, 'main': 'PRIVATE_PAGE' * 10000, 'configuration': {'secret': 'PRIVATE_API_KEY'}}
if a.question == 'fallback':
    report['parent_result'] = {'excerpt': 'Bounded evidence.', 'source': a.url, 'fallback': {'stage': 'answer', 'reason': 'timeout'}}
    report['evidence_spans'] = [{'id': 'p0', 'start': 1, 'end': 20}]
f.write_text(json.dumps(report))
print('UNBOUNDED_STDOUT_MUST_NOT_ESCAPE' * 1000)
'''


class Client:
    def __init__(self, directory, timeout=3, concurrent=2, config=None, usage=False):
        self.directory = pathlib.Path(directory)
        self.worker = self.directory / 'fake.py'
        self.worker.write_text(WORKER)
        self.artifacts = self.directory / 'jobs'
        command = [sys.executable, str(PATH), '--worker', str(self.worker), '--artifacts-dir', str(self.artifacts), '--timeout', str(timeout), '--max-concurrent', str(concurrent)]
        if usage:
            command.extend(['--usage-dir', str(self.directory / 'usage')])
        else:
            command.append('--no-usage-log')
        if config:
            command.extend(['--config', str(config)])
        self.process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.messages = queue.Queue()
        self.reader = threading.Thread(target=self.read, daemon=True)
        self.reader.start()

    def read(self):
        for line in self.process.stdout:
            self.messages.put(json.loads(line))

    def send(self, method, params=None, request_id=None):
        message = {'jsonrpc': '2.0', 'method': method}
        if params is not None:
            message['params'] = params
        if request_id is not None:
            message['id'] = request_id
        self.raw(json.dumps(message).encode() + b'\n')

    def raw(self, value):
        self.process.stdin.write(value)
        self.process.stdin.flush()

    def receive(self, timeout=3):
        return self.messages.get(timeout=timeout)

    def initialize(self):
        self.send('initialize', {'protocolVersion': '2025-11-25', 'capabilities': {}, 'clientInfo': {'name': 'test', 'version': '1'}}, 1)
        response = self.receive()
        assert response['result']['protocolVersion'] == '2025-11-25'
        self.send('notifications/initialized')

    def call(self, question='question', request_id=2, **extra):
        self.send('tools/call', {'name': 'brw_ask', 'arguments': {'url': 'https://example.test/page', 'question': question, **extra}}, request_id)

    def wait_started(self):
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            if list(self.artifacts.glob('*/started')):
                return
            time.sleep(.01)
        raise AssertionError('worker did not start')

    def close(self):
        self.process.stdin.close()
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait()
            raise
        finally:
            self.reader.join(timeout=2)
            self.process.stdout.close()
            self.process.stderr.close()


class ReaderMCPTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.clients = []

    def tearDown(self):
        for client in self.clients:
            client.close()
        self.directory.cleanup()

    def client(self, **kwargs):
        client = Client(self.directory.name, **kwargs)
        self.clients.append(client)
        return client

    def test_discovery_and_lifecycle(self):
        client = self.client()
        client.send('tools/list', request_id=0)
        self.assertEqual(client.receive()['error']['code'], -32000)
        client.initialize()
        client.send('tools/list', request_id=2)
        tools = client.receive()['result']['tools']
        self.assertEqual([tool['name'] for tool in tools], ['brw_ask'])
        self.assertEqual(set(tools[0]['inputSchema']['properties']), {'url', 'question'})
        self.assertFalse(tools[0]['inputSchema']['additionalProperties'])
        client.send('ping', request_id=3)
        self.assertEqual(client.receive(), {'jsonrpc': '2.0', 'id': 3, 'result': {}})
        client.send('unknown', request_id=4)
        self.assertEqual(client.receive()['error']['code'], -32601)

    def test_bounded_worker_packet_and_private_artifacts(self):
        client = self.client()
        client.initialize()
        client.call()
        result = client.receive()['result']
        encoded = json.dumps(result)
        self.assertLess(len(encoded), 1000)
        self.assertNotIn('PRIVATE_', encoded)
        self.assertNotIn('UNBOUNDED', encoded)
        packet = json.loads(result['content'][0]['text'])
        self.assertEqual(packet['answer'], 'Grounded answer.')
        self.assertEqual(packet['trace']['source_chars'], 40000)
        self.assertEqual(packet['trace']['classifier'], 'select')
        self.assertEqual(packet['source'], 'https://example.test/page')
        reports = list(client.artifacts.glob('*/report.json'))
        self.assertEqual(len(reports), 1)
        if os.name == 'posix':
            self.assertEqual(reports[0].parent.stat().st_mode & 0o077, 0)

    def test_fallback_is_a_normal_bounded_wire_result_and_classified_usage(self):
        client = self.client(usage=True)
        client.initialize()
        client.call('fallback')
        result = client.receive()['result']
        self.assertNotIn('isError', result)
        self.assertNotIn('PRIVATE', json.dumps(result))
        packet = json.loads(result['content'][0]['text'])
        self.assertEqual(packet['excerpt'], 'Bounded evidence.')
        self.assertNotIn('answer', packet)
        self.assertEqual(packet['fallback'], {'stage': 'answer', 'reason': 'timeout'})
        self.assertEqual(packet['trace']['evidence_spans'], [{'id': 'p0', 'start': 1, 'end': 20}])
        path = client.directory/'usage/reader.jsonl'
        deadline = time.monotonic()+1
        while not path.exists() and time.monotonic() < deadline:
            time.sleep(.01)
        row = json.loads(path.read_text().splitlines()[-1])
        self.assertEqual(row['outcome'], 'success')
        self.assertEqual(row['fallback_stage'], 'answer')
        self.assertEqual(row['fallback_reason'], 'timeout')

    def test_model_and_command_arguments_are_rejected(self):
        client = self.client()
        client.initialize()
        for index, override in enumerate(('answer_model', 'config', 'worker', 'out'), 10):
            client.call(request_id=index, **{override: 'untrusted'})
            self.assertEqual(client.receive()['error']['code'], -32602)
        self.assertFalse(list(client.artifacts.glob('*/started')))

    def test_url_and_question_validation(self):
        for url in ('file:///etc/passwd', 'javascript:alert(1)', 'https://u:p@example.test', 'https://example.test/\ntext', 'https://example.test:bad', 'https://'):
            with self.subTest(url=url), self.assertRaises(ValueError):
                ADAPTER.validate_arguments({'url': url, 'question': 'question'})
        for question in ('', '  ', 'x'*2001, None, '\x00'):
            with self.subTest(question=str(question)[:20]), self.assertRaises(ValueError):
                ADAPTER.validate_arguments({'url': 'https://example.test', 'question': question})

    def test_failure_details_do_not_escape(self):
        client = self.client()
        client.initialize()
        client.call('failure')
        result = client.receive()['result']
        self.assertTrue(result['isError'])
        self.assertNotIn('PRIVATE', json.dumps(result))
        self.assertEqual(json.loads(result['content'][0]['text'])['error'], 'worker_failed')
        diagnostic = next(client.artifacts.glob('*/adapter.json'))
        self.assertEqual(json.loads(diagnostic.read_text())['returncode'], 1)
        self.assertEqual(json.loads(diagnostic.read_text())['error_kind'], 'worker_failed')
        self.assertIn('PRIVATE_API_KEY', diagnostic.with_name('stderr.txt').read_text())

    def test_oversized_report_and_answer_are_errors(self):
        client = self.client()
        client.initialize()
        for index, question in enumerate(('oversized-report', 'oversized-answer'), 2):
            client.call(question, index)
            result = client.receive()['result']
            self.assertTrue(result['isError'])
            self.assertLess(len(json.dumps(result)), 1000)

    def test_ping_and_capacity_while_running(self):
        client = self.client(concurrent=1)
        client.initialize()
        client.call('sleep:.3')
        client.wait_started()
        client.send('ping', request_id=3)
        self.assertEqual(client.receive()['id'], 3)
        client.call(request_id=4)
        self.assertTrue(client.receive()['result']['isError'])
        self.assertEqual(client.receive()['id'], 2)

    def test_long_call_progress_ping_and_cancellation(self):
        client = self.client(timeout=15)
        client.initialize()
        client.send('tools/call', {'name': 'brw_ask', 'arguments': {'url': 'https://example.test/page', 'question': 'sleep:12'}, '_meta': {'progressToken': 'opaque-job'}}, request_id=2)
        client.wait_started()
        client.send('ping', request_id=3)
        self.assertEqual(client.receive()['id'], 3)
        packet = client.receive(timeout=7)
        self.assertEqual(packet['method'], 'notifications/progress')
        self.assertEqual(packet['params']['progressToken'], 'opaque-job')
        self.assertGreater(packet['params']['progress'], 0)
        self.assertEqual(set(packet['params']), {'progressToken', 'progress', 'message'})
        client.send('notifications/cancelled', {'requestId': 2})
        deadline = time.monotonic()+3
        while time.monotonic() < deadline and not list(client.artifacts.glob('*/adapter.json')):
            time.sleep(.01)
        self.assertTrue(json.loads(next(client.artifacts.glob('*/adapter.json')).read_text())['cancelled'])
        client.send('ping', request_id=4)
        self.assertEqual(client.receive()['id'], 4)
        with self.assertRaises(queue.Empty):
            client.receive(timeout=.2)

    def test_cancellation_suppresses_reply_and_preserves_cleanup(self):
        client = self.client()
        client.initialize()
        client.call('sleep:.3')
        client.wait_started()
        client.send('notifications/cancelled', {'requestId': 2})
        client.send('ping', request_id=3)
        self.assertEqual(client.receive()['id'], 3)
        with self.assertRaises(queue.Empty):
            client.receive(timeout=.5)
        adapters = list(client.artifacts.glob('*/adapter.json'))
        self.assertEqual(len(adapters), 1)
        self.assertTrue(json.loads(adapters[0].read_text())['cancelled'])
        self.assertEqual(len(list(client.artifacts.glob('*/cleanup'))), 1)

    def test_timeout_allows_worker_finally_cleanup(self):
        client = self.client(timeout=.15)
        client.initialize()
        client.call('sleep:2')
        result = client.receive()['result']
        self.assertTrue(result['isError'])
        self.assertEqual(json.loads(result['content'][0]['text'])['error'], 'deadline_exceeded')
        if os.name == 'posix':
            self.assertEqual(len(list(client.artifacts.glob('*/cleanup'))), 1)

    def test_concurrent_ids_remain_correlated(self):
        client = self.client()
        client.initialize()
        client.call('sleep:.3', 2)
        client.call('question', 3)
        responses = [client.receive(), client.receive()]
        self.assertEqual([r['id'] for r in responses], [3, 2])
        packets = [json.loads(r['result']['content'][0]['text']) for r in responses]
        self.assertNotEqual(packets[0]['trace']['id'], packets[1]['trace']['id'])

    def test_malformed_messages_and_notifications(self):
        client = self.client()
        client.raw(b'{broken}\n')
        self.assertEqual(client.receive()['error']['code'], -32700)
        client.raw(b'[]\n')
        self.assertEqual(client.receive()['error']['code'], -32600)
        client.initialize()
        client.send('notifications/cancelled', {'requestId': []})
        client.send('unknown-notification')
        client.send('ping', request_id=9)
        self.assertEqual(client.receive()['id'], 9)

    def test_oversized_message_closes_connection(self):
        client = self.client()
        client.raw(b'x'*(ADAPTER.MAX_MESSAGE+1)+b'\n')
        self.assertEqual(client.receive()['error']['code'], -32600)
        self.assertEqual(client.process.wait(timeout=3), 0)

    def test_version_negotiation_is_honest(self):
        args = argparse_namespace()
        output = io.StringIO()
        server = ADAPTER.Server(args, output)
        server.dispatch({'jsonrpc': '2.0', 'id': 1, 'method': 'initialize', 'params': {'protocolVersion': '2026-07-28', 'capabilities': {}, 'clientInfo': {}}})
        self.assertEqual(json.loads(output.getvalue())['result']['protocolVersion'], '2025-11-25')

    def test_operator_config_forwarded_without_shell(self):
        config = pathlib.Path(self.directory.name) / 'config with spaces.json'
        config.write_text('{}')
        client = self.client(config=config)
        client.initialize()
        client.call()
        self.assertNotIn('isError', client.receive()['result'])
        self.assertEqual(next(client.artifacts.glob('*/config-argument')).read_text(), str(config))

    def test_stderr_capture_is_bounded(self):
        client = self.client()
        client.initialize()
        client.call('stderr-flood')
        self.assertNotIn('isError', client.receive()['result'])
        self.assertEqual(next(client.artifacts.glob('*/stderr.txt')).stat().st_size, 8192)

    def test_eof_waits_for_worker_cleanup_without_reply(self):
        client = self.client()
        client.initialize()
        client.call('sleep:.2')
        client.wait_started()
        client.close()
        self.clients.remove(client)
        self.assertEqual(len(list(client.artifacts.glob('*/cleanup'))), 1)
        self.assertTrue(json.loads(next(client.artifacts.glob('*/adapter.json')).read_text())['cancelled'])
        self.assertTrue(client.messages.empty())

    def test_completeness_packet_is_bounded_and_preserves_ranges(self):
        report = {'parent_result': {'answer': 'Answer', 'source': 'https://example.test'}, 'source_chars': 100, 'source_total_chars': 200, 'source_truncated': True, 'evidence_narrowed': True, 'source_sha256': 'a'*64, 'evidence_spans': [{'id': 'p0', 'start': 5, 'end': 20}]}
        trace = ADAPTER.bounded_result(report, 'test', 1)['trace']
        self.assertEqual(trace['source_total_chars'], 200)
        self.assertTrue(trace['source_truncated'])
        self.assertTrue(trace['evidence_narrowed'])
        self.assertEqual(trace['evidence_spans'], report['evidence_spans'])
        report['evidence_spans'] = [{'start': i, 'end': i+1} for i in range(100)]
        limited = ADAPTER.bounded_result(report, 'test', 1)['trace']
        self.assertEqual(len(limited['evidence_spans']), 16)
        self.assertEqual(limited['evidence_span_count'], 100)
        self.assertTrue(limited['evidence_spans_truncated'])
        report['evidence_spans'] = [{'start': -1, 'end': 5}]
        self.assertNotIn('evidence_spans', ADAPTER.bounded_result(report, 'test', 1)['trace'])

    def test_fallback_wire_contract_preserves_excerpt_and_evidence(self):
        fallback = {'stage': 'answer', 'reason': 'timeout'}
        report = {'parent_result': {'excerpt': 'Bounded evidence.', 'source': 'https://example.test', 'fallback': fallback}, 'source_chars': 100, 'source_total_chars': 200, 'source_truncated': True, 'evidence_spans': [{'id': 'p0', 'start': 1, 'end': 20}]}
        packet = ADAPTER.bounded_result(report, 'test', 1)
        self.assertEqual(packet['fallback'], fallback)
        self.assertIn('excerpt', packet)
        self.assertNotIn('answer', packet)
        self.assertEqual(packet['trace']['evidence_spans'], report['evidence_spans'])
        self.assertEqual(packet['trace']['source_total_chars'], 200)
        for invalid in ({'stage': 'PRIVATE', 'reason': 'timeout'}, {'stage': 'answer', 'reason': 'PRIVATE'}, {'stage': 'answer', 'reason': 'timeout', 'detail': 'PRIVATE'}, [], None):
            report['parent_result']['fallback'] = invalid
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                ADAPTER.bounded_result(report, 'test', 1)
        report['parent_result'] = {'answer': 'Must not label fallback generated.', 'source': 'https://example.test', 'fallback': fallback}
        with self.assertRaises(ValueError):
            ADAPTER.bounded_result(report, 'test', 1)

    @unittest.skipUnless(os.name == 'posix', 'POSIX reader process-group cancellation')
    def test_real_optional_model_child_is_cancelled_without_fallback_delivery(self):
        from test_browser_answer_worker import ProviderFixture
        provider = ProviderFixture([(200, b'{"model":"writer","choices":[]}', 0, .03)])
        self.addCleanup(provider.close)
        root = pathlib.Path(self.directory.name)
        binary = root/'fixture-brw'
        binary.write_text('#!'+sys.executable+'\nimport json,sys\ncommand=sys.argv[1]\nresponses={"health":{"ok":True,"identity":{"headless":True},"version":"fixture"},"open":{"tab":{"id":"owned"},"ready":True},"read":{"url":"https://example.test/page","main":"Owned fixture evidence."},"tab":{"ok":True}}\nprint(json.dumps(responses[command]))\n')
        binary.chmod(0o700)
        config = root/'config.json'
        config.write_text(json.dumps({'brw': str(binary), 'answer_model': 'writer', 'answer_endpoint': provider.endpoint, 'answer_key_env': None, 'request_timeout': 5}))
        client = self.client(config=config, concurrent=1, usage=True, timeout=15)
        actual = pathlib.Path(__file__).with_name('browser-answer-worker.py').resolve()
        client.worker.write_text('import runpy\nrunpy.run_path('+repr(str(actual))+',run_name="__main__")\n')
        client.initialize()
        client.call(request_id=2)
        self.assertTrue(provider.started.wait(10))
        client.send('notifications/cancelled', {'requestId': 2})
        deadline = time.monotonic()+3
        while time.monotonic() < deadline and not list(client.artifacts.glob('*/adapter.json')):
            time.sleep(.01)
        diagnostic = json.loads(next(client.artifacts.glob('*/adapter.json')).read_text())
        self.assertTrue(diagnostic['cancelled'])
        self.assertTrue(client.messages.empty())
        self.assertFalse(list(client.artifacts.glob('*/report.json')))
        self.assertTrue(provider.closed.wait(1))
        client.send('ping', request_id=4)
        self.assertEqual(client.receive()['id'], 4)
        rows = [json.loads(line) for line in (root/'usage/reader.jsonl').read_text().splitlines()]
        self.assertTrue(any(row['outcome']=='cancelled' for row in rows))
        self.assertNotIn('fallback_reason', rows[-1])

    def test_cancellation_interrupts_and_retains_capacity_through_cleanup(self):
        delayed = WORKER.replace("(f.parent / 'cleanup').write_text('yes')", "time.sleep(.2)\n        (f.parent / 'cleanup').write_text('yes')")
        with patch.object(sys.modules[__name__], 'WORKER', delayed):
            client = self.client(concurrent=1, usage=True)
        client.initialize()
        client.call('sleep:5', 2)
        client.wait_started()
        started = time.monotonic()
        client.send('notifications/cancelled', {'requestId': 2})
        client.call('question', 3)
        self.assertTrue(client.receive()['result']['isError'])
        deadline = time.monotonic()+2
        while time.monotonic() < deadline and not list(client.artifacts.glob('*/adapter.json')):
            time.sleep(.01)
        self.assertLess(time.monotonic()-started, 2)
        self.assertEqual(len(list(client.artifacts.glob('*/cleanup'))), 1)
        self.assertTrue(client.messages.empty())
        client.call('question', 4)
        self.assertEqual(client.receive()['id'], 4)
        path = client.directory / 'usage/reader.jsonl'
        deadline = time.monotonic()+2
        cancelled = None
        rows = []
        while time.monotonic() < deadline:
            if path.exists():
                rows = [json.loads(line) for line in path.read_text().splitlines()]
                cancelled = next((row for row in rows if row['outcome'] == 'cancelled'), None)
                if cancelled:
                    break
            time.sleep(.01)
        self.assertIsNotNone(cancelled)
        self.assertEqual(cancelled['output_bytes'], 0)
        self.assertGreater(cancelled['cleanup_ms'], 100)
        self.assertGreater(cancelled['input_bytes'], 0)
        encoded = json.dumps(rows)
        for secret in ('PRIVATE', 'https://example', 'sleep:', 'Grounded answer'):
            self.assertNotIn(secret, encoded)

    def test_windows_cleanup_uses_owned_process_tree_and_bounds_failures(self):
        server = ADAPTER.Server(argparse_namespace(), io.StringIO())
        process = Mock(pid=987654, returncode=None)
        process.poll.return_value = None
        with patch.object(ADAPTER.os, 'name', 'nt'), patch.object(ADAPTER.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0)) as run:
            server.stop_process(process)
        self.assertEqual(run.call_args.args[0], ['taskkill', '/PID', '987654', '/T', '/F'])
        self.assertEqual(run.call_args.kwargs['timeout'], 5)
        process.wait.assert_called_once_with(timeout=1)
        for failure in (FileNotFoundError(), subprocess.TimeoutExpired('taskkill', 5), subprocess.CompletedProcess([], 1)):
            process.reset_mock()
            with self.subTest(failure=failure):
                kwargs = {'side_effect': failure} if isinstance(failure, Exception) else {'return_value': failure}
                with patch.object(ADAPTER.os, 'name', 'nt'), patch.object(ADAPTER.subprocess, 'run', **kwargs), self.assertRaises(RuntimeError):
                    server.stop_process(process)
                process.kill.assert_called_once()
                process.wait.assert_called_once_with(timeout=1)

    def test_darwin_group_probe_permission_race_waits_for_leader_exit(self):
        server = ADAPTER.Server(argparse_namespace(), io.StringIO())
        process = Mock(pid=987654, returncode=None)
        process.poll.return_value = None
        process.wait.return_value = -2
        with patch.object(ADAPTER.os, 'name', 'posix'), patch.object(ADAPTER.sys, 'platform', 'darwin'), patch.object(ADAPTER.os, 'killpg', side_effect=[None, PermissionError(1, 'probe')]) as kill:
            server.stop_process(process)
        process.wait.assert_called_once_with(timeout=.01)
        self.assertEqual(kill.call_args_list[-1].args, (process.pid, 0))

    def test_group_probe_permission_failure_with_live_leader_is_preserved(self):
        server = ADAPTER.Server(argparse_namespace(), io.StringIO())
        for platform in ('darwin', 'linux'):
            with self.subTest(platform=platform):
                process = Mock(pid=987654, returncode=None)
                process.poll.return_value = None
                process.wait.side_effect = subprocess.TimeoutExpired('owned worker', .01)
                with patch.object(ADAPTER.os, 'name', 'posix'), patch.object(ADAPTER.sys, 'platform', platform), patch.object(ADAPTER.os, 'killpg', side_effect=[None, PermissionError(1, 'probe')]):
                    with self.assertRaises(PermissionError):
                        server.stop_process(process)
                if platform == 'linux':
                    process.wait.assert_not_called()

    def test_cleanup_failure_records_duration_and_private_diagnostic(self):
        worker = pathlib.Path(self.directory.name) / 'fake.py'
        worker.write_text(WORKER)
        for cancelled in (False, True):
            with self.subTest(cancelled=cancelled):
                args = ADAPTER.parse_args(['--worker', str(worker), '--artifacts-dir', self.directory.name, '--no-usage-log'])
                server = ADAPTER.Server(args, io.StringIO())
                job = {'cancelled': cancelled, 'cancel_event': threading.Event(), 'input_bytes': 123}
                if cancelled:
                    job['cancel_event'].set()
                server.active[2] = job
                with patch.object(server, 'stop_process', side_effect=PermissionError(1, 'cleanup failed')), patch.object(server.ledger, 'write') as write:
                    server.run_worker(2, 'https://example.test', 'question', job)
                fields = write.call_args.kwargs
                self.assertIsNotNone(fields['cleanup_ms'])
                self.assertGreaterEqual(fields['cleanup_ms'], 0)
                diagnostics = [json.loads(path.read_text()) for path in pathlib.Path(self.directory.name).glob('*/adapter.json')]
                self.assertTrue(any(item['error_kind'] == 'reader_failed' and 'cleanup failed' in item['error_detail'] for item in diagnostics))
                self.assertNotIn(2, server.active)

    def test_failure_usage_survives_missing_success_report(self):
        client = self.client(usage=True)
        client.initialize()
        client.call('failure')
        self.assertTrue(client.receive()['result']['isError'])
        deadline = time.monotonic()+1
        path = client.directory / 'usage/reader.jsonl'
        while time.monotonic() < deadline and not path.exists():
            time.sleep(.01)
        row = json.loads(path.read_text().splitlines()[0])
        self.assertEqual(row['operation'], 'adapter')
        self.assertEqual(row['outcome'], 'error')
        self.assertGreater(row['output_bytes'], 0)
        self.assertNotIn('PRIVATE', json.dumps(row))

    def test_nonfinite_json_is_rejected(self):
        with self.assertRaises(ValueError):
            ADAPTER.decode('{"value": NaN}')


def argparse_namespace():
    import argparse
    return argparse.Namespace(max_concurrent=2)


if __name__ == '__main__':
    unittest.main()
