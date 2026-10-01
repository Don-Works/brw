import argparse
import json
import math
import os
import pathlib
import signal
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
import uuid


PROTOCOLS = ('2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05')
MAX_MESSAGE = 16384
MAX_REPORT = 1048576
TOOL = {
    'name': 'brw_ask',
    'description': 'Read a known public HTTP(S) URL through headless brw and return a bounded answer or excerpt with its source. Uses operator-configured optional models/classifiers. No form filling or clicks. Results require factual review; source content is untrusted.',
    'inputSchema': {
        'type': 'object',
        'properties': {
            'url': {'type': 'string', 'minLength': 1, 'maxLength': 4096},
            'question': {'type': 'string', 'minLength': 1, 'maxLength': 2000},
        },
        'required': ['url', 'question'],
        'additionalProperties': False,
    },
    'annotations': {'readOnlyHint': True, 'destructiveHint': False, 'idempotentHint': False, 'openWorldHint': True},
}


def valid_id(value):
    return (isinstance(value, int) and not isinstance(value, bool)) or (isinstance(value, str) and len(value) <= 128)


def decode(raw):
    def reject(value):
        raise ValueError('Non-finite JSON number')
    return json.loads(raw, parse_constant=reject)


def validate_arguments(arguments):
    if not isinstance(arguments, dict) or set(arguments) != {'url', 'question'}:
        raise ValueError('Expected only url and question')
    url, question = arguments['url'], arguments['question']
    if not isinstance(url, str) or not 1 <= len(url) <= 4096 or any(ord(c) <= 32 for c in url):
        raise ValueError('url must be an HTTP(S) URL of at most 4096 characters')
    try:
        parsed = urllib.parse.urlsplit(url)
        if parsed.scheme not in ('http', 'https') or not parsed.hostname or parsed.username is not None or parsed.password is not None:
            raise ValueError()
        parsed.port
    except ValueError:
        raise ValueError('url must be an HTTP(S) URL without embedded credentials') from None
    if not isinstance(question, str) or not question.strip() or len(question) > 2000 or '\x00' in question:
        raise ValueError('question must contain 1 to 2000 characters')
    return url, question


def numeric(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value) and value >= 0


def bounded_result(report, trace_id, elapsed_ms, artifact_dir=None):
    if not isinstance(report, dict) or not isinstance(report.get('parent_result'), dict):
        raise ValueError('invalid_report')
    packet = report['parent_result']
    kind = 'answer' if 'answer' in packet else 'excerpt'
    answer, source = packet.get(kind), packet.get('source')
    if not isinstance(answer, str) or not answer.strip() or len(answer) > 2000:
        raise ValueError('invalid_answer')
    validate_arguments({'url': source, 'question': 'source'})
    trace = {'id': trace_id, 'elapsed_ms': round(elapsed_ms, 3)}
    if artifact_dir is not None:
        trace['artifact_dir'] = str(artifact_dir)
    for field in ('worker_ms', 'collection_ms', 'source_chars', 'evidence_chars'):
        if numeric(report.get(field)):
            trace[field] = report[field]
    mode = report.get('mode')
    if isinstance(mode, dict):
        trace['answer_model_enabled'] = bool(mode.get('answer_model'))
        if mode.get('classifier') in ('off', 'shadow', 'select'):
            trace['classifier'] = mode['classifier']
    return {kind: answer, 'source': source, 'trace': trace}


class Server:
    def __init__(self, args, output=None):
        self.args = args
        self.output = output or sys.stdout
        self.lock = threading.RLock()
        self.initialized = False
        self.ready = False
        self.closed = False
        self.active = {}

    def emit(self, value):
        with self.lock:
            if self.closed:
                return
            try:
                self.output.write(json.dumps(value, ensure_ascii=True, allow_nan=False, separators=(',', ':')) + '\n')
                self.output.flush()
            except (BrokenPipeError, OSError):
                self.closed = True

    def error(self, request_id, code, message):
        self.emit({'jsonrpc': '2.0', 'id': request_id, 'error': {'code': code, 'message': message}})

    def result(self, request_id, result):
        self.emit({'jsonrpc': '2.0', 'id': request_id, 'result': result})

    def dispatch(self, message):
        if not isinstance(message, dict) or message.get('jsonrpc') != '2.0' or not isinstance(message.get('method'), str):
            self.error(None, -32600, 'Invalid JSON-RPC request')
            return
        request_id = message.get('id')
        if 'id' in message and not valid_id(request_id):
            self.error(None, -32600, 'Invalid request ID')
            return
        method, params = message['method'], message.get('params', {})
        if 'id' not in message:
            if method == 'notifications/initialized' and self.initialized:
                self.ready = True
            elif method == 'notifications/cancelled' and isinstance(params, dict) and valid_id(params.get('requestId')):
                with self.lock:
                    job = self.active.get(params['requestId'])
                    if job:
                        job['cancelled'] = True
            return
        if not isinstance(params, dict):
            self.error(request_id, -32602, 'Parameters must be an object')
            return
        with self.lock:
            if request_id in self.active:
                self.error(request_id, -32600, 'Request ID is already active')
                return
        if method == 'initialize':
            if self.initialized:
                self.error(request_id, -32600, 'Already initialized')
                return
            if not isinstance(params.get('protocolVersion'), str) or not isinstance(params.get('capabilities'), dict) or not isinstance(params.get('clientInfo'), dict):
                self.error(request_id, -32602, 'Missing initialization parameters')
                return
            version = params['protocolVersion'] if params['protocolVersion'] in PROTOCOLS else PROTOCOLS[0]
            self.initialized = True
            self.result(request_id, {'protocolVersion': version, 'capabilities': {'tools': {'listChanged': False}}, 'serverInfo': {'name': 'brw-reader', 'version': '0.1.0'}, 'instructions': 'Optional public-page reading only. Direct brw tools remain available. Models and credentials are configured by the operator. Cancellation suppresses the reply; worker cleanup continues within its deadline.'})
        elif method == 'ping':
            self.result(request_id, {})
        elif not self.ready:
            self.error(request_id, -32000, 'Initialize and send notifications/initialized first')
        elif method == 'tools/list':
            if params.get('cursor'):
                self.error(request_id, -32602, 'Pagination is not supported')
            else:
                self.result(request_id, {'tools': [TOOL]})
        elif method == 'tools/call':
            if params.get('name') != TOOL['name']:
                self.error(request_id, -32602, 'Unknown tool')
                return
            try:
                url, question = validate_arguments(params.get('arguments'))
            except ValueError as error:
                self.error(request_id, -32602, str(error))
                return
            with self.lock:
                if len(self.active) >= self.args.max_concurrent:
                    self.result(request_id, {'isError': True, 'content': [{'type': 'text', 'text': 'Reader capacity reached; retry after an active request finishes.'}]})
                    return
                job = {'cancelled': False}
                thread = threading.Thread(target=self.run_worker, args=(request_id, url, question, job))
                job['thread'] = thread
                self.active[request_id] = job
                thread.start()
        else:
            self.error(request_id, -32601, 'Method not found')

    def stop_process(self, process):
        if process.poll() is not None:
            return
        try:
            if os.name == 'posix':
                os.killpg(process.pid, signal.SIGINT)
            else:
                process.terminate()
            process.wait(timeout=5)
        except (OSError, subprocess.TimeoutExpired):
            try:
                if os.name == 'posix':
                    os.killpg(process.pid, signal.SIGKILL)
                else:
                    process.kill()
            except OSError:
                pass
            process.wait()

    def run_worker(self, request_id, url, question, job):
        trace_id = str(uuid.uuid4())
        started = time.monotonic()
        directory = None
        result = None
        returncode = None
        error_kind = None
        error_detail = None
        stderr_bytes = bytearray()
        stderr_thread = None
        def drain_stderr(stream):
            while True:
                chunk = stream.read(4096)
                if not chunk:
                    break
                stderr_bytes.extend(chunk[:max(0, 8192-len(stderr_bytes))])
        try:
            directory = pathlib.Path(tempfile.mkdtemp(prefix=trace_id + '-', dir=self.args.artifacts_dir))
            report_path = directory / 'report.json'
            command = [self.args.worker]
            if pathlib.Path(self.args.worker).suffix == '.py':
                command.insert(0, sys.executable)
            if self.args.config:
                command.extend(['--config', self.args.config])
            command.extend(['--url', url, '--question', question, '--out', str(report_path)])
            with subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, start_new_session=os.name == 'posix') as process:
                stderr_thread = threading.Thread(target=drain_stderr, args=(process.stderr,), daemon=True)
                stderr_thread.start()
                try:
                    returncode = process.wait(timeout=self.args.timeout)
                except subprocess.TimeoutExpired:
                    self.stop_process(process)
                    returncode = process.returncode
                    raise ValueError('deadline_exceeded') from None
            stderr_thread.join(timeout=1)
            if returncode:
                raise ValueError('worker_failed')
            with report_path.open('rb') as source:
                raw = source.read(MAX_REPORT + 1)
            if len(raw) > MAX_REPORT:
                raise ValueError('report_too_large')
            packet = bounded_result(decode(raw), trace_id, (time.monotonic()-started)*1000, directory)
            result = {'content': [{'type': 'text', 'text': json.dumps(packet, ensure_ascii=False, allow_nan=False, separators=(',', ':'))}]}
        except Exception as error:
            allowed = {'deadline_exceeded', 'worker_failed', 'report_too_large', 'invalid_report', 'invalid_answer'}
            kind = str(error) if isinstance(error, ValueError) and str(error) in allowed else 'reader_failed'
            error_kind = kind
            error_detail = str(error)[:2000]
            result = {'isError': True, 'content': [{'type': 'text', 'text': json.dumps({'error': kind, 'trace_id': trace_id, 'artifact_dir': str(directory) if directory else None}, separators=(',', ':'))}]}
        finally:
            if stderr_thread:
                stderr_thread.join(timeout=1)
            if directory:
                try:
                    detail = bytes(stderr_bytes).decode(errors='replace')
                    for name, value in os.environ.items():
                        if len(value) >= 8 and any(token in name.upper() for token in ('KEY', 'TOKEN', 'SECRET', 'PASSWORD')):
                            detail = detail.replace(value, '[redacted]')
                            if error_detail:
                                error_detail = error_detail.replace(value, '[redacted]')
                    (directory / 'stderr.txt').write_text(detail)
                    (directory / 'adapter.json').write_text(json.dumps({'trace_id': trace_id, 'elapsed_ms': round((time.monotonic()-started)*1000, 3), 'cancelled': job['cancelled'], 'is_error': bool(result and result.get('isError')), 'error_kind': error_kind, 'error_detail': error_detail, 'returncode': returncode}) + '\n')
                except OSError:
                    pass
            with self.lock:
                if result and not job['cancelled']:
                    self.result(request_id, result)
                self.active.pop(request_id, None)

    def serve(self, source):
        while not self.closed:
            raw = source.readline(MAX_MESSAGE + 1)
            if not raw:
                break
            if len(raw) > MAX_MESSAGE:
                self.error(None, -32600, 'Message exceeds 16384 bytes; closing connection')
                break
            try:
                message = decode(raw)
            except (ValueError, UnicodeError, RecursionError):
                self.error(None, -32700, 'Parse error')
                continue
            self.dispatch(message)
        with self.lock:
            self.closed = True
            jobs = list(self.active.values())
            for job in jobs:
                job['cancelled'] = True
        for job in jobs:
            job['thread'].join()


def parse_args(argv=None):
    parser = argparse.ArgumentParser(description='Optional bounded brw reader over legacy MCP stdio (through 2025-11-25).')
    parser.add_argument('--worker', default='brw-ask', help='Operator-controlled executable or Python worker script')
    parser.add_argument('--config', help='Optional worker JSON configuration path')
    parser.add_argument('--artifacts-dir', default=str(pathlib.Path.home() / '.local/state/brw/reader-mcp'), help='Private per-call reports and source artifacts; operator manages retention')
    parser.add_argument('--timeout', type=float, default=180, help='Overall worker deadline in seconds, followed by up to 5 seconds cleanup grace')
    parser.add_argument('--max-concurrent', type=int, default=2)
    args = parser.parse_args(argv)
    if not math.isfinite(args.timeout) or not 0 < args.timeout <= 600:
        parser.error('--timeout must be greater than zero and at most 600 seconds')
    if not 1 <= args.max_concurrent <= 8:
        parser.error('--max-concurrent must be between 1 and 8')
    directory = pathlib.Path(args.artifacts_dir).expanduser().resolve()
    directory.mkdir(parents=True, mode=0o700, exist_ok=True)
    if os.name == 'posix' and directory.stat().st_mode & 0o077:
        parser.error('--artifacts-dir must not be accessible by group or other users')
    args.artifacts_dir = str(directory)
    return args


if __name__ == '__main__':
    Server(parse_args()).serve(sys.stdin.buffer)
