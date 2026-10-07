import contextlib
import importlib.util
import io
import http.server
import os
import threading
import time
import urllib.error
import json
import pathlib
import random
import uuid
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location('worker', pathlib.Path(__file__).with_name('browser-answer-worker.py'))
worker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(worker)


class ProviderFixture:
    def __init__(self, frames):
        fixture = self
        self.frames = list(frames)
        self.started = threading.Event()
        self.closed = threading.Event()
        self.requests = []
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                fixture.requests.append(json.loads(self.rfile.read(int(self.headers['Content-Length']))))
                fixture.started.set()
                status, body, delay, drip, *provider_ids = fixture.frames.pop(0)
                time.sleep(delay)
                self.send_response(status)
                self.send_header('Content-Length', str(len(body)))
                if provider_ids:
                    self.send_header('x-request-id', provider_ids[0])
                self.end_headers()
                try:
                    if drip:
                        for byte in body:
                            self.wfile.write(bytes([byte]))
                            self.wfile.flush()
                            time.sleep(drip)
                    else:
                        self.wfile.write(body)
                except (BrokenPipeError, ConnectionResetError):
                    pass
                finally:
                    fixture.closed.set()
            def log_message(self, *args):
                pass
        self.server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.endpoint = 'http://127.0.0.1:'+str(self.server.server_port)+'/chat'
    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=1)


class WorkerTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = pathlib.Path(self.directory.name)
        self.source = self.root / 'source.json'
        self.page = {'url': 'https://example.test/source', 'main': 'Johns Hopkins was a merchant and philanthropist who funded a university and a hospital.\n\nAnother passage with more than sixty characters supplies background and unrelated historical details.'}
        self.source.write_text(json.dumps(self.page))
        self.base = ['--no-usage-log', '--source-artifact', str(self.source), '--question', 'Who was Johns Hopkins?', '--out', str(self.root/'report.json'), '--classifier-key-env', '']

    def test_default_has_no_model_or_classifier_calls(self):
        args = worker.parse_args(self.base)
        self.assertFalse(args.usage_log)
        with patch.object(worker, 'optional_http') as call:
            result = worker.run(args)
        call.assert_not_called()
        self.assertIn('excerpt', result)
        self.assertNotIn('answer', result)
        report = json.loads((self.root/'report.json').read_text())
        self.assertIsNone(report['source_total_chars'])
        self.assertIsNone(report['source_truncated'])

    def test_shadow_failure_preserves_baseline_evidence(self):
        args = worker.parse_args(self.base + ['--classifier-mode', 'shadow'])
        baseline = worker.run(worker.parse_args(self.base))
        with patch.object(worker, 'optional_http', side_effect=TimeoutError('fixture')):
            result = worker.run(args)
        self.assertEqual(result['excerpt'], baseline['excerpt'])
        self.assertEqual(result['fallback'], {'stage': 'classifier', 'reason': 'timeout'})
        report = json.loads((self.root/'report.json').read_text())
        self.assertEqual(report['phases']['classifier']['fallback']['reason'], 'timeout')

    def test_custom_config_and_cli_override(self):
        config = self.root / 'config.json'
        config.write_text(json.dumps({'answer_model': 'cloud-model', 'answer_endpoint': 'https://cloud.test/chat', 'classifier_model': 'cloud-classifier', 'classifier_endpoint': 'https://cloud.test/decide', 'request_timeout': 12, 'classifier_key_env': None}))
        args = worker.parse_args(self.base + ['--config', str(config), '--answer-model', 'override-model'])
        self.assertEqual(args.answer_model, 'override-model')
        self.assertEqual(args.answer_endpoint, 'https://cloud.test/chat')
        self.assertEqual(args.classifier_endpoint, 'https://cloud.test/decide')
        self.assertEqual(args.request_timeout, 12)

    def test_chat_classifier_and_answer_receive_configured_models(self):
        args = worker.parse_args(self.base + ['--classifier-mode', 'select', '--classifier-protocol', 'openai-chat', '--classifier-model', 'selector', '--classifier-endpoint', 'http://selector.test/chat', '--answer-model', 'writer', '--answer-endpoint', 'http://writer.test/chat', '--evidence-max-chars', '70'])
        selector = {'model': 'selector', 'choices': [{'message': {'content': '{"choice":"p0"}'}}]}
        answer = {'model': 'writer', 'choices': [{'message': {'content': 'Johns Hopkins funded a university and a hospital.'}, 'finish_reason': 'stop'}]}
        with patch.object(worker, 'optional_http', side_effect=[(selector, {}), (answer, {})]) as call:
            result = worker.run(args)
        self.assertEqual(call.call_args_list[0].args[0], 'http://selector.test/chat')
        self.assertEqual(call.call_args_list[0].args[1]['model'], 'selector')
        self.assertEqual(call.call_args_list[1].args[1]['model'], 'writer')
        evidence = json.loads(call.call_args_list[1].args[1]['messages'][-1]['content'])['evidence']
        self.assertNotIn('Another passage', evidence)
        self.assertLessEqual(len(evidence), 70)
        self.assertIn('funded', result['answer'])

    def test_unknown_candidate_and_truncated_answer_fall_back(self):
        args = worker.parse_args(self.base + ['--classifier-mode', 'select'])
        with patch.object(worker, 'optional_http', return_value=({'model': args.classifier_model, 'answers': {'passage': {'choice': 'invented'}}}, {})):
            result = worker.run(args)
            self.assertIn('excerpt', result)
            self.assertEqual(result['fallback']['reason'], 'unknown_candidate')
        args = worker.parse_args(self.base + ['--answer-model', 'writer'])
        response = {'model': 'writer', 'choices': [{'message': {'content': 'Partial'}, 'finish_reason': 'length'}]}
        with patch.object(worker, 'optional_http', return_value=(response, {})):
            result = worker.run(args)
            self.assertIn('excerpt', result)
            self.assertEqual(result['fallback']['reason'], 'truncated_response')

    def test_optional_failures_restore_baseline_excerpt_and_ranges(self):
        baseline = worker.run(worker.parse_args(self.base))
        baseline_report = json.loads((self.root/'report.json').read_text())
        for stage, flags in [('classifier', ['--classifier-mode', 'select']), ('answer', ['--answer-model', 'writer'])]:
            with self.subTest(stage=stage):
                with patch.object(worker, 'optional_http', side_effect=TimeoutError('PRIVATE_PROVIDER_ERROR')):
                    result = worker.run(worker.parse_args(self.base + flags))
                self.assertEqual(result['excerpt'], baseline['excerpt'])
                self.assertNotIn('answer', result)
                self.assertEqual(result['fallback'], {'stage': stage, 'reason': 'timeout'})
                self.assertNotIn('PRIVATE', json.dumps(result))
                report = json.loads((self.root/'report.json').read_text())
                self.assertEqual(report['evidence_spans'], baseline_report['evidence_spans'])
                self.assertEqual(report['source_sha256'], baseline_report['source_sha256'])

    def test_unknown_model_identity_restores_excerpt(self):
        response = {'model': 'unexpected-model', 'choices': [{'message': {'content': 'Answer'}, 'finish_reason': 'stop'}]}
        with patch.object(worker, 'optional_http', return_value=(response, {})):
            result = worker.run(worker.parse_args(self.base + ['--answer-model', 'writer']))
        self.assertIn('excerpt', result)
        self.assertEqual(result['fallback'], {'stage': 'answer', 'reason': 'unknown_identity'})

    def test_missing_model_identity_and_invalid_answers_fall_back(self):
        cases = [
            ({'choices': [{'message': {'content': 'Answer'}}]}, 'unknown_identity'),
            ({'model': 'writer', 'choices': []}, 'invalid_response'),
            ({'model': 'writer', 'choices': [{'message': {'content': ''}}]}, 'empty_response'),
            ({'model': 'writer', 'choices': [{'message': {'content': 'PRIVATE'*200}}]}, 'oversized_response'),
        ]
        for response, reason in cases:
            with self.subTest(reason=reason), patch.object(worker, 'optional_http', return_value=(response, {})):
                result = worker.run(worker.parse_args(self.base + ['--answer-model', 'writer']))
                self.assertEqual(result['fallback']['reason'], reason)
                self.assertNotIn('PRIVATE', json.dumps(result))
                self.assertNotIn('answer', result)

    def provider(self, frames):
        provider = ProviderFixture(frames)
        self.addCleanup(provider.close)
        return provider

    def test_real_provider_success_preserves_answer_and_usage(self):
        response = {'model': 'writer', 'choices': [{'message': {'content': 'Grounded fixture.'}, 'finish_reason': 'stop'}], 'usage': {'prompt_tokens': 10, 'completion_tokens': 3}}
        provider = self.provider([(200, json.dumps(response).encode(), 0, 0)])
        args = worker.parse_args(self.base + ['--answer-model', 'writer', '--answer-endpoint', provider.endpoint, '--answer-key-env', 'BRW_FIXTURE_AUTH'])
        args.usage_log, args.usage_dir = True, str(self.root/'usage')
        with patch.dict(os.environ, {'BRW_FIXTURE_AUTH': 'FIXTURE_VALID_CREDENTIAL_ABC123'}):
            result = worker.run(args)
        self.assertEqual(result, {'answer': 'Grounded fixture.', 'source': self.page['url']})
        rows = [json.loads(line) for line in (self.root/'usage/reader.jsonl').read_text().splitlines()]
        model = next(row for row in rows if row['scope']=='model')
        self.assertEqual(model['provider_input_tokens'], 10)
        self.assertEqual(model['provider_output_tokens'], 3)
        self.assertNotIn('Grounded', json.dumps(rows))
        self.assertNotIn('http://', json.dumps(rows))

    def test_real_provider_failures_are_fixed_and_private(self):
        cases = [(503, b'PRIVATE_PROVIDER_ERROR', 'http'), (200, b'not json PRIVATE', 'invalid_response'), (200, b'{"usage":{"prompt_tokens":1e999}}', 'invalid_response'), (200, b'', 'empty_response'), (200, b'x'*1048577, 'oversized_response')]
        for status, raw, reason in cases:
            with self.subTest(reason=reason):
                provider = self.provider([(status, raw, 0, 0)])
                args = worker.parse_args(self.base + ['--answer-model', 'writer', '--answer-endpoint', provider.endpoint])
                result = worker.run(args)
                self.assertEqual(result['fallback'], {'stage': 'answer', 'reason': reason})
                self.assertIn('excerpt', result)
                self.assertNotIn('PRIVATE', json.dumps(result))
                self.assertNotIn('PRIVATE', (self.root/'report.json').read_text())

    def test_reflected_auth_key_is_refused_before_content_or_metadata_persistence(self):
        key = 'FIXTURE_CREDENTIAL_ABC123'
        for reflected in ('answer', 'response_id', 'provider_request_id', 'escaped_response_id'):
            with self.subTest(reflected=reflected):
                response = {'model': 'writer', 'choices': [{'message': {'content': 'Grounded fixture.'}, 'finish_reason': 'stop'}]}
                provider_id = 'fixture-provider'
                if reflected == 'answer':
                    response['choices'][0]['message']['content'] = 'Reflected '+key
                elif reflected == 'provider_request_id':
                    provider_id = key
                else:
                    response['id'] = key
                raw = json.dumps(response).encode()
                if reflected == 'escaped_response_id':
                    raw = raw.replace(key.encode(), b'\\u0046'+key[1:].encode())
                    self.assertNotIn(key.encode(), raw)
                provider = self.provider([(200, raw, 0, 0, provider_id)])
                args = worker.parse_args(self.base + ['--answer-model', 'writer', '--answer-endpoint', provider.endpoint, '--answer-key-env', 'BRW_FIXTURE_AUTH'])
                args.usage_log, args.usage_dir = True, str(self.root/'usage')
                with patch.dict(os.environ, {'BRW_FIXTURE_AUTH': key}):
                    result = worker.run(args)
                self.assertIn('excerpt', result)
                self.assertNotIn('answer', result)
                self.assertEqual(result['fallback'], {'stage': 'answer', 'reason': 'invalid_response'})
                for value in (json.dumps(result), (self.root/'report.json').read_text(), (self.root/'report.json.jsonl').read_text(), (self.root/'usage/reader.jsonl').read_text()):
                    self.assertNotIn(key, value)

    def test_slow_drip_is_bounded_and_child_reaped(self):
        response = {'model': 'writer', 'choices': [{'message': {'content': 'Slow fixture.'}, 'finish_reason': 'stop'}]}
        provider = self.provider([(200, json.dumps(response).encode(), 0, .06)])
        args = worker.parse_args(self.base + ['--answer-model', 'writer', '--answer-endpoint', provider.endpoint, '--request-timeout', '.35'])
        args.usage_log, args.usage_dir = True, str(self.root/'usage')
        children = []
        original = subprocess.Popen
        def launch(*args, **kwargs):
            process = original(*args, **kwargs)
            children.append(process)
            return process
        started = time.monotonic()
        with patch.object(worker.subprocess, 'Popen', side_effect=launch):
            result = worker.run(args)
        self.assertLess(time.monotonic()-started, 1.5)
        self.assertTrue(provider.started.is_set())
        self.assertEqual(result['fallback'], {'stage': 'answer', 'reason': 'timeout'})
        self.assertEqual(len(children), 1)
        self.assertIsNotNone(children[0].returncode)
        self.assertTrue(provider.closed.wait(1))
        rows = [json.loads(line) for line in (self.root/'usage/reader.jsonl').read_text().splitlines()]
        failed = next(row for row in rows if row['scope']=='model')
        self.assertEqual(failed['outcome'], 'error')
        self.assertIsNone(failed['provider_input_tokens'])
        self.assertIsNone(failed['output_bytes'])
        self.assertEqual(rows[-1]['fallback_reason'], 'timeout')

    def test_classifier_and_answer_share_one_deadline(self):
        selector = {'model': 'selector', 'answers': {'passage': {'choice': 'p0'}}}
        answer = {'model': 'writer', 'choices': [{'message': {'content': 'Slow fixture.'}, 'finish_reason': 'stop'}]}
        provider = self.provider([(200, json.dumps(selector).encode(), .25, 0), (200, json.dumps(answer).encode(), 0, .06)])
        args = worker.parse_args(self.base + ['--classifier-mode', 'select', '--classifier-model', 'selector', '--classifier-endpoint', provider.endpoint, '--answer-model', 'writer', '--answer-endpoint', provider.endpoint, '--request-timeout', '2'])
        started = time.monotonic()
        with patch.object(worker, 'optional_http', wraps=worker.optional_http) as calls:
            result = worker.run(args)
        self.assertLess(time.monotonic()-started, 3)
        self.assertEqual(len(provider.requests), 2)
        self.assertEqual(calls.call_args_list[0].args[3], calls.call_args_list[1].args[3])
        self.assertEqual(result['fallback'], {'stage': 'answer', 'reason': 'timeout'})
        baseline = worker.run(worker.parse_args(self.base))
        self.assertEqual(result['excerpt'], baseline['excerpt'])

    def test_classifier_failure_skips_answer_and_missing_key_is_optional(self):
        args = worker.parse_args(self.base + ['--classifier-mode', 'shadow', '--answer-model', 'writer'])
        with patch.object(worker, 'optional_http', side_effect=TimeoutError('PRIVATE')) as call:
            result = worker.run(args)
        self.assertEqual(call.call_count, 1)
        self.assertEqual(result['fallback']['stage'], 'classifier')
        args = worker.parse_args(self.base + ['--answer-model', 'writer', '--answer-key-env', 'BRW_TEST_MISSING_KEY'])
        with patch.dict(os.environ, {}, clear=True), patch.object(worker, 'optional_http') as call:
            result = worker.run(args)
        call.assert_not_called()
        self.assertEqual(result['fallback'], {'stage': 'answer', 'reason': 'unavailable'})

    def test_unbounded_provider_metadata_cannot_break_fallback_report(self):
        response = {'model': 'unexpected', 'id': 'PRIVATE'*50000, 'usage': {'private': 'PRIVATE'*50000}, 'choices': []}
        provider = self.provider([(200, json.dumps(response).encode(), 0, 0)])
        args = worker.parse_args(self.base + ['--answer-model', 'writer', '--answer-endpoint', provider.endpoint])
        result = worker.run(args)
        self.assertEqual(result['fallback']['reason'], 'unknown_identity')
        report = (self.root/'report.json').read_text()
        self.assertLess(len(report), 20000)
        self.assertNotIn('PRIVATE', report)

    def test_cancellation_and_collection_remain_job_errors(self):
        args = worker.parse_args(self.base + ['--answer-model', 'writer'])
        with patch.object(worker, 'optional_http', side_effect=KeyboardInterrupt), self.assertRaises(KeyboardInterrupt):
            worker.run(args)
        with patch.object(worker, 'optional_http', side_effect=worker.OptionalCleanupFailure('cleanup')), self.assertRaises(worker.OptionalCleanupFailure):
            worker.run(args)
        args.source_artifact, args.url = None, 'https://example.test'
        with patch.object(worker, 'collect', side_effect=RuntimeError('collection')), self.assertRaises(RuntimeError):
            worker.run(args)

    def test_none_selection_is_not_a_generated_answer(self):
        args = worker.parse_args(self.base + ['--classifier-mode', 'select', '--answer-model', 'writer'])
        response = {'model': args.classifier_model, 'answers': {'passage': {'choice': 'none'}}}
        with patch.object(worker, 'optional_http', return_value=(response, {})) as call:
            result = worker.run(args)
        self.assertEqual(call.call_count, 1)
        self.assertIn('excerpt', result)
        self.assertNotIn('answer', result)
        self.assertNotIn('fallback', result)
        self.assertIn('does not provide sufficient', result['excerpt'])

    def test_failed_open_closes_only_its_owned_tab(self):
        responses = [{'ok': True, 'identity': {'headless': True}, 'version': 'test'}, {'tab': {'id': 'owned'}, 'ready': True, 'http_status': 403}, {'ok': True}]
        calls = []
        def run(command, **kwargs):
            calls.append((command, kwargs['env']['BRW_OWNER_ID']))
            return subprocess.CompletedProcess(command, 0, json.dumps(responses.pop(0)), '')
        with patch.object(worker.subprocess, 'run', side_effect=run):
            with self.assertRaises(RuntimeError):
                worker.collect('https://example.test', 'http://daemon.test', 'brw')
        self.assertEqual(calls[-1][0][1:3], ['tab', 'close'])
        self.assertEqual(calls[-1][0][-1], 'owned')
        self.assertTrue(calls[0][1].startswith('brw-answer-'))
        self.assertEqual(len({owner for _, owner in calls}), 1)

    def test_short_facts_and_ranked_multi_passage_evidence(self):
        self.page['main'] = 'Approval: FACT_A.\n\nSecond approval: FACT_B.'
        self.source.write_text(json.dumps(self.page))
        args = worker.parse_args(self.base + ['--evidence-mode', 'ranked', '--answer-model', 'writer'])
        def answer(endpoint, body, *arguments, **kwargs):
            evidence = json.loads(body['messages'][-1]['content'])['evidence']
            self.assertIn('FACT_A', evidence)
            self.assertIn('FACT_B', evidence)
            return {'model': 'writer', 'choices': [{'message': {'content': 'Both approvals exist.'}, 'finish_reason': 'stop'}]}, {}
        with patch.object(worker, 'optional_http', side_effect=answer):
            worker.run(args)
        report = json.loads((self.root/'report.json').read_text())
        self.assertEqual(len(report['evidence_spans']), 2)
        for span in report['evidence_spans']:
            self.assertIn('FACT_', self.page['main'][span['start']:span['end']])

    def test_randomized_passage_ranges_and_total_budget(self):
        rng = random.Random(20261001)
        for case in range(100):
            text = '\n\n'.join('α🙂 fact '+str(i)+' '+('text '*rng.randrange(1, 20)) for i in range(rng.randrange(1, 12)))
            candidates, ranges = worker.passage_candidates(text, 'fact', width=rng.randrange(1, 80), limit=rng.randrange(1, 12))
            for key, value in candidates.items():
                self.assertEqual(text[ranges[key]['start']:ranges[key]['end']], value)
            budget = rng.randrange(1, 300)
            evidence, spans = worker.pack_passages(candidates, ranges, budget)
            self.assertLessEqual(len(evidence), budget)
            self.assertEqual(evidence, '\n\n'.join(text[span['start']:span['end']] for span in spans))

    def test_provider_usage_logs_exact_bytes_and_nullable_tokens_without_content(self):
        class Response:
            headers = {'x-request-id': 'private-provider-id'}
            def __enter__(self):
                return self
            def __exit__(self, *args):
                return None
            def read(self, size):
                return raw
        body = {'model': 'private-model-name', 'messages': [{'role': 'user', 'content': 'PRIVATE_PROMPT🙂'}]}
        result = {'choices': [{'message': {'content': 'PRIVATE_RESPONSE'}}], 'usage': {'prompt_tokens': 100, 'completion_tokens': 9, 'prompt_tokens_details': {'cached_tokens': 0}, 'completion_tokens_details': {'reasoning_tokens': 0}}}
        raw = json.dumps(result).encode()
        ledger = worker.USAGE.Ledger(self.root/'usage')
        with patch.object(worker.urllib.request, 'urlopen', return_value=Response()):
            worker.timed_http('https://private-endpoint.test', body, 'PRIVATE_KEY', ledger=ledger, trace_id=str(uuid.uuid4()))
        row = json.loads((self.root/'usage/reader.jsonl').read_text())
        self.assertEqual(row['input_bytes'], len(json.dumps(body).encode()))
        self.assertEqual(row['output_bytes'], len(raw))
        self.assertEqual(row['provider_input_tokens'], 100)
        self.assertEqual(row['provider_output_tokens'], 9)
        self.assertEqual(row['provider_cached_input_tokens'], 0)
        self.assertEqual(row['provider_reasoning_tokens'], 0)
        self.assertIsNone(row['provider_cache_write_tokens'])
        self.assertIsNone(row['first_visible_delta_ms'])
        self.assertNotIn('PRIVATE', json.dumps(row))
        self.assertNotIn('private', json.dumps(row))
        with patch.object(worker.urllib.request, 'urlopen', side_effect=TimeoutError('PRIVATE_FAILURE')):
            with self.assertRaises(TimeoutError):
                worker.timed_http('https://private-endpoint.test', body, ledger=ledger)
        failed = json.loads((self.root/'usage/reader.jsonl').read_text().splitlines()[-1])
        self.assertIsNone(failed['output_bytes'])
        self.assertIsNone(failed['provider_input_tokens'])
        self.assertEqual(failed['outcome'], 'error')
        self.assertNotIn('PRIVATE', json.dumps(failed))

    def test_job_usage_includes_completeness_and_off_switch(self):
        args = worker.parse_args(self.base)
        args.usage_log = True
        args.usage_dir = str(self.root/'usage')
        self.page['main_total_chars'] = 10000
        self.source.write_text(json.dumps(self.page))
        worker.run(args)
        row = json.loads((self.root/'usage/reader.jsonl').read_text())
        self.assertEqual(row['operation'], 'job')
        self.assertTrue(row['source_truncated'])
        self.assertEqual(row['source_total_chars'], 10000)
        self.assertTrue(row['collection_replayed'])
        self.assertIsNone(row['collection_ms'])
        self.assertGreater(row['duration_us'], 0)
        args.usage_dir = str(self.root/'disabled')
        args.usage_log = False
        worker.run(args)
        self.assertFalse((self.root/'disabled').exists())

    def test_unknown_or_fractional_config_fails(self):
        config = self.root / 'config.json'
        for value in [{'typo': True}, {'passage_count': 1.5}, [], {'request_timeout': float('nan')}]:
            config.write_text(json.dumps(value))
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                worker.parse_args(self.base + ['--config', str(config)])


if __name__ == '__main__':
    unittest.main()
