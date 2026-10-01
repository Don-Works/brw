import contextlib
import importlib.util
import io
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
        with patch.object(worker, 'timed_http') as call:
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
        with patch.object(worker, 'timed_http', side_effect=TimeoutError('fixture')):
            result = worker.run(args)
        self.assertEqual(result, baseline)
        report = json.loads((self.root/'report.json').read_text())
        self.assertEqual(report['phases']['classifier']['error_kind'], 'TimeoutError')

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
        selector = {'choices': [{'message': {'content': '{"choice":"p0"}'}}]}
        answer = {'choices': [{'message': {'content': 'Johns Hopkins funded a university and a hospital.'}, 'finish_reason': 'stop'}]}
        with patch.object(worker, 'timed_http', side_effect=[(selector, {}), (answer, {})]) as call:
            result = worker.run(args)
        self.assertEqual(call.call_args_list[0].args[0], 'http://selector.test/chat')
        self.assertEqual(call.call_args_list[0].args[1]['model'], 'selector')
        self.assertEqual(call.call_args_list[1].args[1]['model'], 'writer')
        evidence = json.loads(call.call_args_list[1].args[1]['messages'][-1]['content'])['evidence']
        self.assertNotIn('Another passage', evidence)
        self.assertLessEqual(len(evidence), 70)
        self.assertIn('funded', result['answer'])

    def test_unknown_candidate_and_truncated_answer_fail(self):
        args = worker.parse_args(self.base + ['--classifier-mode', 'select'])
        with patch.object(worker, 'timed_http', return_value=({'answers': {'passage': {'choice': 'invented'}}}, {})):
            with self.assertRaises(ValueError):
                worker.run(args)
        args = worker.parse_args(self.base + ['--answer-model', 'writer'])
        response = {'choices': [{'message': {'content': 'Partial'}, 'finish_reason': 'length'}]}
        with patch.object(worker, 'timed_http', return_value=(response, {})):
            with self.assertRaises(ValueError):
                worker.run(args)

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
            return {'choices': [{'message': {'content': 'Both approvals exist.'}, 'finish_reason': 'stop'}]}, {}
        with patch.object(worker, 'timed_http', side_effect=answer):
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
