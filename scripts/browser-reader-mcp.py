import argparse
import json
import importlib.util
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


_USAGE_SPEC = importlib.util.spec_from_file_location("brw_reader_usage", pathlib.Path(__file__).with_name("browser-reader-usage.py"))
USAGE = importlib.util.module_from_spec(_USAGE_SPEC)
_USAGE_SPEC.loader.exec_module(USAGE)


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


decode = USAGE.decode_json

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
    if isinstance(value, bool):
        return False
    if isinstance(value, int):
        return 0 <= value <= 9223372036854775807
    return isinstance(value, float) and math.isfinite(value) and 0 <= value <= 9223372036854775807


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
    for field in ('worker_ms', 'collection_ms'):
        if numeric(report.get(field)):
            trace[field] = report[field]
    for field in ('source_chars', 'source_total_chars', 'evidence_chars'):
        if USAGE.token_count(report.get(field)) is not None:
            trace[field] = report[field]
    for field in ('source_truncated', 'evidence_narrowed'):
        if isinstance(report.get(field), bool):
            trace[field] = report[field]
    if isinstance(report.get('source_sha256'), str) and len(report['source_sha256']) == 64 and all(c in '0123456789abcdef' for c in report['source_sha256']):
        trace['source_sha256'] = report['source_sha256']
    spans = report.get('evidence_spans')
    if isinstance(spans, list) and len(spans) <= 254:
        validated = []
        for span in spans:
            if not isinstance(span, dict) or not isinstance(span.get('start'), int) or isinstance(span['start'], bool) or not isinstance(span.get('end'), int) or isinstance(span['end'], bool) or not 0 <= span['start'] <= span['end'] <= 9007199254740991:
                break
            if 'source_chars' in trace and span['end'] > trace['source_chars']:
                break
            value = {'start': span['start'], 'end': span['end']}
            if isinstance(span.get('id'), str) and len(span['id']) <= 10 and span['id'].startswith('p') and span['id'][1:].isdigit():
                value['id'] = span['id']
            validated.append(value)
        else:
            trace['evidence_spans'] = validated[:16]
            trace['evidence_span_count'] = len(validated)
            trace['evidence_spans_truncated'] = len(validated) > 16
    mode = report.get('mode')
    if isinstance(mode, dict):
        trace['answer_model_enabled'] = bool(mode.get('answer_model'))
        if mode.get('classifier') in ('off', 'shadow', 'select'):
            trace['classifier'] = mode['classifier']
    result = {kind: answer, 'source': source, 'trace': trace}
    if 'fallback' in packet:
        fallback = packet['fallback']
        if kind != 'excerpt' or not isinstance(fallback, dict) or set(fallback) != {'stage', 'reason'} or fallback['stage'] not in USAGE.FALLBACK_STAGES or fallback['reason'] not in USAGE.FALLBACK_REASONS:
            raise ValueError('invalid_report')
        result['fallback'] = fallback
    return result


class Server:
    def __init__(self, args, output=None):
        self.args = args
        self.output = output or sys.stdout
        self.lock = threading.RLock()
        self.initialized = False
        self.ready = False
        self.closed = False
        self.active = {}
        self.ledger = USAGE.from_args(args) if hasattr(args, 'usage_log') else USAGE.Ledger(enabled=False)

    def emit(self, value):
        with self.lock:
            if self.closed:
                return
            try:
                encoded = json.dumps(value, ensure_ascii=True, allow_nan=False, separators=(',', ':')) + '\n'
                self.output.write(encoded)
                self.output.flush()
                return len(encoded.encode())
            except (BrokenPipeError, OSError):
                self.closed = True

    def error(self, request_id, code, message):
        self.emit({'jsonrpc': '2.0', 'id': request_id, 'error': {'code': code, 'message': message}})

    def result(self, request_id, result):
        return self.emit({'jsonrpc': '2.0', 'id': request_id, 'result': result})

    def dispatch(self, message, input_bytes=None):
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
                        job['cancel_event'].set()
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
            self.result(request_id, {'protocolVersion': version, 'capabilities': {'tools': {'listChanged': False}}, 'serverInfo': {'name': 'brw-reader', 'version': '0.1.0'}, 'instructions': 'Optional public-page reading only. Direct brw tools remain available. Models and credentials are configured by the operator. Cancellation interrupts the worker and suppresses its reply; capacity is released after cleanup completes.'})
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
                meta = params.get('_meta')
                token = meta.get('progressToken') if isinstance(meta, dict) else None
                if not valid_id(token) or len(json.dumps(token)) > 256:
                    token = None
                job = {'cancelled': False, 'cancel_event': threading.Event(), 'input_bytes': input_bytes, 'progress_token': token}
                thread = threading.Thread(target=self.run_worker, args=(request_id, url, question, job))
                job['thread'] = thread
                self.active[request_id] = job
                thread.start()
        else:
            self.error(request_id, -32601, 'Method not found')

    def stop_process(self, process):
        if os.name == 'posix':
            try:
                os.killpg(process.pid, signal.SIGINT)
            except ProcessLookupError:
                process.poll()
                return
            deadline = time.monotonic()+5
            while True:
                process.poll()
                try:
                    os.killpg(process.pid, 0)
                except ProcessLookupError:
                    return
                except PermissionError as error:
                    if sys.platform != 'darwin':
                        raise
                    try:
                        process.wait(timeout=.01)
                    except subprocess.TimeoutExpired:
                        raise error
                    return
                if time.monotonic() >= deadline:
                    try:
                        os.killpg(process.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    process.wait()
                    return
                time.sleep(.01)
        elif os.name == 'nt' and process.poll() is None:
            try:
                result = subprocess.run(['taskkill', '/PID', str(process.pid), '/T', '/F'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
                if result.returncode:
                    raise OSError('Worker process-tree cleanup failed')
                process.wait(timeout=1)
            except (OSError, subprocess.TimeoutExpired):
                if process.poll() is None:
                    process.kill()
                process.wait(timeout=1)
                raise RuntimeError('Worker process-tree cleanup failed') from None
        elif process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()

    def run_worker(self, request_id, url, question, job):
        trace_id = str(uuid.uuid4())
        started = time.monotonic()
        started_at = USAGE.timestamp()
        cleanup_ms = None
        output_bytes = 0
        directory = None
        result = None
        returncode = None
        error_kind = None
        error_detail = None
        fallback = {}
        stderr_bytes = bytearray()
        stderr_thread = None
        stderr_stop = threading.Event()
        def drain_stderr(stream):
            descriptor = stream.fileno()
            if os.name == 'posix':
                os.set_blocking(descriptor, False)
            while not stderr_stop.is_set():
                try:
                    chunk = os.read(descriptor, 4096)
                except BlockingIOError:
                    stderr_stop.wait(.01)
                    continue
                except OSError:
                    return
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
            environment = dict(os.environ, BRW_READER_TRACE_ID=trace_id, BRW_READER_USAGE_DIR=str(self.ledger.directory), BRW_READER_USAGE_ENABLED='1' if self.ledger.enabled else '0', BRW_READER_USAGE_PARENT='1', BRW_READER_USAGE_MAX_BYTES=str(self.ledger.max_bytes), BRW_READER_USAGE_KEEP=str(self.ledger.keep))
            with subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, bufsize=0, start_new_session=os.name == 'posix', env=environment) as process:
                stderr_thread = threading.Thread(target=drain_stderr, args=(process.stderr,), daemon=True)
                stderr_thread.start()
                deadline = started+self.args.timeout
                next_progress = started+5
                while process.poll() is None:
                    cancelled = job['cancel_event'].is_set()
                    if cancelled or time.monotonic() >= deadline:
                        cleanup_started = time.monotonic()
                        try:
                            self.stop_process(process)
                        finally:
                            cleanup_ms = round((time.monotonic()-cleanup_started)*1000, 3)
                        returncode = process.returncode
                        raise ValueError('cancelled' if cancelled else 'deadline_exceeded')
                    now = time.monotonic()
                    if job.get('progress_token') is not None and now >= next_progress:
                        with self.lock:
                            if not job['cancelled']:
                                self.emit({'jsonrpc': '2.0', 'method': 'notifications/progress', 'params': {'progressToken': job['progress_token'], 'progress': now-started, 'message': 'Elapsed seconds waiting for reader'}})
                        next_progress = now+5
                    try:
                        process.wait(timeout=min(.05, max(.001, deadline-time.monotonic())))
                    except subprocess.TimeoutExpired:
                        pass
                returncode = process.returncode
                cleanup_started = time.monotonic()
                try:
                    self.stop_process(process)
                finally:
                    cleanup_ms = round((time.monotonic()-cleanup_started)*1000, 3)
                stderr_thread.join(timeout=.1)
                stderr_stop.set()
                stderr_thread.join(timeout=.1)
            stderr_thread.join(timeout=1)
            if returncode:
                raise ValueError('worker_failed')
            with report_path.open('rb') as source:
                raw = source.read(MAX_REPORT + 1)
            if len(raw) > MAX_REPORT:
                raise ValueError('report_too_large')
            packet = bounded_result(decode(raw), trace_id, (time.monotonic()-started)*1000, directory)
            fallback = packet.get('fallback', {})
            result = {'content': [{'type': 'text', 'text': json.dumps(packet, ensure_ascii=False, allow_nan=False, separators=(',', ':'))}]}
        except Exception as error:
            allowed = {'deadline_exceeded', 'worker_failed', 'report_too_large', 'invalid_report', 'invalid_answer', 'cancelled'}
            kind = str(error) if isinstance(error, ValueError) and str(error) in allowed else 'reader_failed'
            error_kind = kind
            error_detail = str(error)[:2000]
            result = {'isError': True, 'content': [{'type': 'text', 'text': json.dumps({'error': kind, 'trace_id': trace_id, 'artifact_dir': str(directory) if directory else None}, separators=(',', ':'))}]}
        finally:
            stderr_stop.set()
            if stderr_thread:
                stderr_thread.join(timeout=.2)
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
                    output_bytes = self.result(request_id, result)
                self.active.pop(request_id, None)
            elapsed = time.monotonic()-started
            self.ledger.write('adapter', 'transport', trace_id, job_id=trace_id, started_at=started_at, finished_at=USAGE.timestamp(), duration_ms=round(elapsed*1000, 3), duration_us=round(elapsed*1000000), input_bytes=job.get('input_bytes'), output_bytes=output_bytes, representation='jsonrpc', outcome='cancelled' if job['cancelled'] else 'deadline_exceeded' if error_kind == 'deadline_exceeded' else 'error' if error_kind else 'success', cancelled=job['cancelled'], cleanup_ms=cleanup_ms, fallback_stage=fallback.get('stage'), fallback_reason=fallback.get('reason'))

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
            self.dispatch(message, len(raw))
        with self.lock:
            self.closed = True
            jobs = list(self.active.values())
            for job in jobs:
                job['cancelled'] = True
                job['cancel_event'].set()
        for job in jobs:
            job['thread'].join()


def parse_args(argv=None):
    parser = argparse.ArgumentParser(description='Optional bounded brw reader over legacy MCP stdio (through 2025-11-25).')
    parser.add_argument('--worker', default='brw-ask', help='Operator-controlled executable or Python worker script')
    parser.add_argument('--config', help='Optional worker JSON configuration path')
    parser.add_argument('--artifacts-dir', default=str(pathlib.Path.home() / '.local/state/brw/reader-mcp'), help='Private per-call reports and source artifacts; operator manages retention')
    parser.add_argument('--timeout', type=float, default=180, help='Overall worker deadline in seconds, followed by up to 5 seconds cleanup grace')
    parser.add_argument('--max-concurrent', type=int, default=2)
    USAGE.add_arguments(parser)
    known, _ = parser.parse_known_args(argv)
    if known.config:
        configuration = decode(pathlib.Path(known.config).read_text())
        if not isinstance(configuration, dict):
            parser.error('Configuration must be a JSON object')
        parser.set_defaults(**{key: value for key, value in configuration.items() if key in {'usage_dir', 'usage_log', 'usage_max_bytes', 'usage_keep'}})
    args = parser.parse_args(argv)
    USAGE.validate(args, parser)
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
