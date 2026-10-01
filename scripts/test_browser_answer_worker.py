import contextlib
import importlib.util
import io
import json
import pathlib
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
        self.base = ['--source-artifact', str(self.source), '--question', 'Who was Johns Hopkins?', '--out', str(self.root/'report.json'), '--classifier-key-env', '']

    def test_default_has_no_model_or_classifier_calls(self):
        args = worker.parse_args(self.base)
        with patch.object(worker, 'timed_http') as call:
            result = worker.run(args)
        call.assert_not_called()
        self.assertIn('excerpt', result)
        self.assertNotIn('answer', result)

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

    def test_unknown_or_fractional_config_fails(self):
        config = self.root / 'config.json'
        for value in [{'typo': True}, {'passage_count': 1.5}, [], {'request_timeout': float('nan')}]:
            config.write_text(json.dumps(value))
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                worker.parse_args(self.base + ['--config', str(config)])


if __name__ == '__main__':
    unittest.main()
