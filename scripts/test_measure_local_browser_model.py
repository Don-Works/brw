import importlib.util
import io
import json
import pathlib
import struct
import sys
import tempfile
import unittest
import uuid
import urllib.error
import zlib
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

    def test_provider_nonfinite_numbers_are_response_json_failures(self):
        with patch.object(bench.urllib.request, 'urlopen', return_value=Response(b'{"choices":[],"usage":{"prompt_tokens":NaN}}')):
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

    def test_model_identity_mismatch_cannot_pass(self):
        case = bench.cases()[0]
        call = {'function': {'name': case['expected']['name'], 'arguments': json.dumps(case['expected']['arguments'])}}
        answer = self.answer([call])
        answer['model'] = 'different-model'
        row, _, _ = self.run_answer(answer, case)
        self.assertTrue(row['proposal_correct'])
        self.assertFalse(row['pass'])
        self.assertEqual(row['outcome'], 'model_identity_mismatch')

    def test_invalid_provider_tokens_are_unknown(self):
        answer = self.answer(finish='length')
        answer['usage'] = {'prompt_tokens': True, 'completion_tokens': -5, 'completion_tokens_details': {'reasoning_tokens': True}}
        row, _, _ = self.run_answer(answer)
        self.assertIsNone(row['provider_input_tokens'])
        self.assertIsNone(row['provider_output_tokens'])
        self.assertIsNone(row['provider_reasoning_tokens'])
        self.assertEqual(row['outcome'], 'output_budget_exhausted')

    def test_credentials_redacted_from_success_metadata_too(self):
        key = 'fixture-' + uuid.uuid4().hex
        answer = self.answer(content=key)
        answer['usage']['debug'] = key
        with patch.object(bench.urllib.request, 'urlopen', return_value=Response(json.dumps(answer).encode())):
            row = bench.request('http://localhost/v1', 'fixture-model', bench.cases()[0], api_key=key)
        self.assertNotIn(key, json.dumps(row))


class VisionFixtureTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = pathlib.Path(self.directory.name)
        def chunk(kind, data):
            return struct.pack('>I', len(data)) + kind + data + struct.pack('>I', zlib.crc32(kind + data))
        pixels = b''.join(b'\0' + b'\x25\x63\xeb' * 20 for _ in range(20))
        self.png = b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', 20, 20, 8, 2, 0, 0, 0)) + chunk(b'IDAT', zlib.compress(pixels)) + chunk(b'IEND', b'')
        (self.root / 'fixture.png').write_bytes(self.png)
        self.case = {'name': 'visual-target', 'goal': 'Choose the current blue visual target.', 'image': 'fixture.png', 'observation': [], 'expected': {'name': 'brw_click_xy', 'arguments': {'x': 10, 'y': 10}}, 'target_region': {'x': 5, 'y': 5, 'width': 10, 'height': 10, 'shape': 'diamond'}}
        self.manifest = {'schema_version': 1, 'source_policy': 'owned_fixture', 'cases': [self.case]}
        self.path = self.root / 'fixtures.json'
        self.identity = {'schema_version': 1, 'model': 'fixture-model', 'runtime': {'name': 'fixture-runtime', 'version': '1.0', 'backend': 'fixture', 'device': 'fixture'}, 'checkpoint': {'publisher': 'fixture', 'revision': 'fixture-revision', 'quantization': 'none', 'adapter': 'none', 'sha256': '1' * 64}, 'tokenizer': {'revision': 'fixture-tokenizer', 'chat_template_sha256': '2' * 64}}
        self.identity_path = self.root / 'identity.json'
        self.identity_path.write_text(json.dumps(self.identity))

    def load(self):
        self.path.write_text(json.dumps(self.manifest))
        return bench.vision_cases(self.path)

    def answer(self, x=10, y=10):
        return {'id': 'fixture', 'model': 'fixture-model', 'system_fingerprint': 'fixture-runtime-observation', 'choices': [{'message': {'tool_calls': [{'function': {'name': 'brw_click_xy', 'arguments': json.dumps({'x': x, 'y': y})}}]}, 'finish_reason': 'tool_calls'}], 'usage': {'prompt_tokens': 800, 'completion_tokens': 20}}

    def test_image_payload_hides_grading_answers_and_records_timing(self):
        case = self.load()[0]
        events = []
        with patch.object(bench.urllib.request, 'urlopen', return_value=Response(json.dumps(self.answer()).encode())) as call:
            row = bench.request('http://localhost/v1', 'fixture-model', case, event=events.append, runtime_identity=self.identity, request_phase='first_request')
        body = json.loads(call.call_args.args[0].data)
        content = body['messages'][-1]['content']
        self.assertEqual(content[1]['type'], 'image_url')
        self.assertTrue(content[1]['image_url']['url'].startswith('data:image/png;base64,'))
        prompt = json.loads(content[0]['text'])
        self.assertNotIn('expected', prompt)
        self.assertNotIn('target_region', prompt)
        self.assertTrue(row['pass'])
        self.assertEqual(row['provider_input_tokens'], 800)
        self.assertEqual(row['provider_output_tokens'], 20)
        self.assertTrue(row['estimate_excludes_image_tokens'])
        self.assertEqual(row['request_phase'], 'first_request')
        self.assertEqual(row['runtime_identity_source'], 'operator_manifest')
        self.assertFalse(row['runtime_identity_verified'])
        self.assertEqual(row['system_fingerprint'], 'fixture-runtime-observation')
        for key in ['fixture_preprocess_ms', 'request_preprocess_ms', 'serialization_ms', 'whole_response_ms', 'end_to_end_ms']:
            self.assertGreaterEqual(row[key], 0)
        self.assertIsNone(row['ttft_ms'])
        self.assertNotIn('data:image/png;base64,', json.dumps(events))

    def test_geometry_accepts_hits_and_rejects_wrong_shape_or_types(self):
        case = self.load()[0]
        for x, y, expected in [(10, 10, True), (14, 10, True), (14, 14, False), (20, 10, False), (True, 10, False), (float('inf'), 10, False)]:
            actual = [{'name': 'brw_click_xy', 'arguments': {'x': x, 'y': y}}]
            self.assertEqual(bench.correct_proposal(actual, case), expected)
        self.assertFalse(bench.correct_proposal([{'name': 'brw_click_xy', 'arguments': None}], case))
        self.assertFalse(bench.correct_proposal([case['expected'], case['expected']], case))

    def test_css_mapping_is_in_prompt_and_grading(self):
        self.case['image_transform'] = {'viewport_x': 100, 'viewport_y': 200, 'css_per_image_pixel': 2}
        self.case['expected']['arguments'] = {'x': 120, 'y': 220}
        case = self.load()[0]
        self.assertTrue(bench.correct_proposal([self.case['expected']], case))
        self.assertFalse(bench.correct_proposal([{'name': 'brw_click_xy', 'arguments': {'x': 10, 'y': 10}}], case))
        with patch.object(bench.urllib.request, 'urlopen', return_value=Response(json.dumps(self.answer(120, 220)).encode())) as call:
            row = bench.request('http://localhost/v1', 'fixture-model', case)
        prompt = json.loads(json.loads(call.call_args.args[0].data)['messages'][-1]['content'][0]['text'])
        self.assertEqual(prompt['image_transform'], self.case['image_transform'])
        self.assertTrue(row['pass'])

    def test_policy_schema_and_geometry_fail_before_network(self):
        for change in [lambda: self.manifest.update(source_policy='private_account'), lambda: self.manifest.update(schema_version=True), lambda: self.case['target_region'].update(width=99), lambda: self.case.update(image_transform={'viewport_x': 0, 'viewport_y': 0, 'css_per_image_pixel': True}), lambda: self.case['expected'].update(arguments={'x': 10, 'y': 10, 'private': 'no'}), lambda: self.manifest['cases'].append(dict(self.case))]:
            original = json.dumps(self.manifest)
            change()
            with self.assertRaises(ValueError), patch.object(bench.urllib.request, 'urlopen') as network:
                self.load()
            network.assert_not_called()
            self.manifest = json.loads(original)
            self.case = self.manifest['cases'][0]

    def test_image_traversal_symlinks_and_damage_are_rejected(self):
        self.case['image'] = '../outside.png'
        with self.assertRaises(ValueError):
            self.load()
        with tempfile.TemporaryDirectory() as outside:
            target = pathlib.Path(outside) / 'outside.png'
            target.write_bytes(self.png)
            (self.root / 'link.png').symlink_to(target)
            self.case['image'] = 'link.png'
            with self.assertRaises(ValueError):
                self.load()
        self.case['image'] = 'fixture.png'
        (self.root / 'fixture.png').write_bytes(self.png[:-3])
        with self.assertRaises(ValueError):
            self.load()
        damaged = bytearray(self.png)
        damaged[30] ^= 1
        (self.root / 'fixture.png').write_bytes(damaged)
        with self.assertRaises(ValueError):
            self.load()

    def test_identity_manifest_requires_exact_pin(self):
        self.assertEqual(bench.identity_manifest(self.identity_path, 'fixture-model'), self.identity)
        with self.assertRaises(ValueError):
            bench.identity_manifest(self.identity_path, 'other-model')
        self.identity['checkpoint']['sha256'] = 'unknown'
        self.identity_path.write_text(json.dumps(self.identity))
        with self.assertRaises(ValueError):
            bench.identity_manifest(self.identity_path, 'fixture-model')

    def test_plan_only_freezes_manifest_without_credentials_or_network(self):
        self.load()
        output = self.root / 'report.json'
        argv = ['measure', '--model', 'fixture-model', '--vision-fixtures', str(self.path), '--identity-manifest', str(self.identity_path), '--out', str(output), '--plan-only', '--api-key-env', 'ABSENT_FIXTURE_CREDENTIAL']
        with patch.object(sys, 'argv', argv), patch.object(bench.urllib.request, 'urlopen') as network, patch('sys.stdout', new=io.StringIO()):
            bench.main()
        network.assert_not_called()
        report = json.loads(output.read_text())
        frozen = json.loads(pathlib.Path(str(output) + '.manifest.json').read_text())
        self.assertFalse(report['summary']['inference_executed'])
        self.assertFalse(frozen['effects_executed'])
        self.assertFalse(frozen['model_loaded_by_harness'])
        self.assertEqual(frozen['first_request_state'], 'unknown')
        self.assertEqual(frozen['selection']['selected_ids'], ['visual-target'])
        self.assertEqual(frozen['runtime_identity'], self.identity)
        self.assertEqual(frozen['resource_limits']['planned_requests'], 2)
        self.assertNotIn('data:image/png;base64,', json.dumps(report))

    def test_wrong_visual_point_is_quality_failure_not_transport(self):
        case = self.load()[0]
        with patch.object(bench.urllib.request, 'urlopen', return_value=Response(json.dumps(self.answer(14, 14)).encode())):
            row = bench.request('http://localhost/v1', 'fixture-model', case)
        self.assertFalse(row['pass'])
        self.assertEqual(row['outcome'], 'wrong_arguments')

    def test_reference_point_outside_region_invalidates_fixture(self):
        self.case['expected']['arguments'] = {'x': 19, 'y': 19}
        with self.assertRaises(ValueError):
            self.load()

    def test_mocked_run_freezes_before_requests_and_keeps_usage_unknown(self):
        self.load()
        output = self.root / 'report.json'
        frozen_path = pathlib.Path(str(output) + '.manifest.json')
        answer = self.answer()
        answer['usage'] = {'prompt_tokens': True}
        def response(*args, **kwargs):
            self.assertTrue(frozen_path.is_file(), 'requests began before the experiment was frozen')
            return Response(json.dumps(answer).encode())
        argv = ['measure', '--model', 'fixture-model', '--vision-fixtures', str(self.path), '--identity-manifest', str(self.identity_path), '--out', str(output)]
        with patch.object(sys, 'argv', argv), patch.object(bench.urllib.request, 'urlopen', side_effect=response) as network, patch('sys.stdout', new=io.StringIO()):
            bench.main()
        self.assertEqual(network.call_count, 2)
        report = json.loads(output.read_text())
        self.assertEqual(report['cold']['request_phase'], 'first_request')
        self.assertEqual(report['results'][0]['request_phase'], 'subsequent_request')
        self.assertIsNone(report['summary']['prompt_tokens_total'])
        self.assertIsNone(report['summary']['output_tokens_total'])
        self.assertEqual(report['summary']['usage_missing_requests'], 1)
        self.assertEqual(report['summary']['passed'], 1)
        self.assertNotIn('data:image/png;base64,', output.read_text())
        self.assertNotIn('data:image/png;base64,', pathlib.Path(str(output) + '.jsonl').read_text())


if __name__ == '__main__':
    unittest.main()
