import importlib.util
import io
import json
import os
import pathlib
import queue
import subprocess
import sys
import tempfile
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
f.write_text(json.dumps(report))
print('UNBOUNDED_STDOUT_MUST_NOT_ESCAPE' * 1000)
'''


class Client:
    def __init__(self, directory, timeout=3, concurrent=2, config=None):
        self.directory = pathlib.Path(directory)
        self.worker = self.directory / 'fake.py'
        self.worker.write_text(WORKER)
        self.artifacts = self.directory / 'jobs'
        command = [sys.executable, str(PATH), '--worker', str(self.worker), '--artifacts-dir', str(self.artifacts), '--timeout', str(timeout), '--max-concurrent', str(concurrent)]
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

    def test_nonfinite_json_is_rejected(self):
        with self.assertRaises(ValueError):
            ADAPTER.decode('{"value": NaN}')


def argparse_namespace():
    import argparse
    return argparse.Namespace(max_concurrent=2)


if __name__ == '__main__':
    unittest.main()
