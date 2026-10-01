import importlib.util
import io
import json
import pathlib
import sys
import unittest
import uuid
import urllib.error
from unittest.mock import patch

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location('bench', pathlib.Path(__file__).with_name('measure-local-browser-model.py'))
bench = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bench)


class Response(io.BytesIO):
    status = 200
    headers = {'x-request-id': 'provider-fixture'}


class DiagnosticsTest(unittest.TestCase):
    def answer(self, calls=None, content='', finish='stop', reasoning=0):
        return {'id': 'fixture', 'model': 'fixture-model', 'choices': [{'message': {'content': content, 'tool_calls': calls or []}, 'finish_reason': finish}], 'usage': {'prompt_tokens': 700, 'completion_tokens': 32, 'completion_tokens_details': {'reasoning_tokens': reasoning}}}

    def run_answer(self, answer, case=None):
        events = []
        with patch.object(bench.urllib.request, 'urlopen', return_value=Response(json.dumps(answer).encode())) as call:
            row = bench.request('http://localhost/v1', 'fixture-model', case or bench.cases()[0], structured=True, reasoning_effort='none', event=events.append)
        return row, events, json.loads(call.call_args.args[0].data)

    def test_correct_call_and_observation_value_survive(self):
        case = bench.cases()[5]
        call = {'function': {'name': 'brw_press', 'arguments': '{"key":"Enter"}'}}
        row, events, body = self.run_answer(self.answer([call], finish='tool_calls'), case)
        self.assertTrue(row['pass'])
        self.assertEqual(row['outcome'], 'correct')
        observation = json.loads(body['messages'][-1]['content'])['observation']
        self.assertIn('Merkle tree', [item.get('value') for item in observation])
        self.assertEqual(body['reasoning_effort'], 'none')
        self.assertEqual(events[0]['event'], 'request_started')
        self.assertEqual(events[1]['event'], 'request_finished')
        self.assertEqual(events[0]['request_id'], events[1]['request_id'])
        self.assertEqual(row['provider_request_id'], 'provider-fixture')
        self.assertIsNone(row['ttft_ms'])
        self.assertGreater(row['response_bytes'], 0)

    def test_reasoning_cap_is_not_a_wrong_target(self):
        row, _, _ = self.run_answer(self.answer(finish='length', reasoning=32))
        self.assertEqual(row['outcome'], 'reasoning_budget_exhausted')
        self.assertFalse(row['pass'])

    def test_prose_call_is_not_native_tool_success(self):
        row, _, _ = self.run_answer(self.answer(content='brw_fill(ref="e12", value="Merkle tree")'))
        self.assertEqual(row['outcome'], 'text_instead_of_tool')
        self.assertFalse(row['pass'])

    def test_extra_call_is_not_ignored(self):
        case = bench.cases()[0]
        call = {'function': {'name': case['expected']['name'], 'arguments': json.dumps(case['expected']['arguments'])}}
        row, _, _ = self.run_answer(self.answer([call, call]))
        self.assertEqual(row['outcome'], 'multiple_tool_calls')
        self.assertFalse(row['pass'])

    def test_invalid_arguments_and_response_json_are_distinct(self):
        row, _, _ = self.run_answer(self.answer([{'function': {'name': 'brw_fill', 'arguments': 'not JSON'}}]))
        self.assertEqual(row['outcome'], 'tool_arguments_error')
        with patch.object(bench.urllib.request, 'urlopen', return_value=Response(b'not JSON')):
            row = bench.request('http://localhost/v1', 'fixture-model', bench.cases()[0])
        self.assertEqual(row['outcome'], 'response_json_error')

    def test_transport_failure_is_not_model_quality_failure(self):
        events = []
        with patch.object(bench.urllib.request, 'urlopen', side_effect=TimeoutError('timed out')):
            row = bench.request('http://localhost/v1', 'fixture-model', bench.cases()[0], event=events.append)
        self.assertEqual(row['outcome'], 'transport_error')
        self.assertEqual(events[-1]['event'], 'request_finished')
        self.assertNotIn('usage', row)

    def test_context_window_error_is_classified_and_credentials_redacted(self):
        key = 'fixture-' + uuid.uuid4().hex
        body = json.dumps({'error': {'code': 'context_length_exceeded', 'message': key}}).encode()
        error = urllib.error.HTTPError('https://fixture.test', 400, 'Bad request', {}, io.BytesIO(body))
        with patch.object(bench.urllib.request, 'urlopen', side_effect=error):
            row = bench.request('https://fixture.test', 'fixture-model', bench.cases()[0], api_key=key)
        self.assertEqual(row['outcome'], 'context_window_exceeded')
        self.assertNotIn(key, row['error'])


if __name__ == '__main__':
    unittest.main()
