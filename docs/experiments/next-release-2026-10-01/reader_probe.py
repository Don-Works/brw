import importlib.util
import io
import json
import pathlib
import queue
import random
import re
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
ROOT = pathlib.Path(__file__).resolve().parents[3]
OUT = pathlib.Path('/tmp/brw-next-reader-probe.json')

def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod

worker = load('probe_worker', ROOT / 'scripts/browser-answer-worker.py')
adapter = load('probe_adapter', ROOT / 'scripts/browser-reader-mcp.py')
fixtures = load('probe_fixtures', ROOT / 'scripts/test_browser_reader_mcp.py')
METRICS = {}

class Probe(unittest.TestCase):
    def test_randomized_evidence_locations(self):
        rng = random.Random(20261001)
        totals = {str(n): {'hits': 0, 'cases': 60, 'evidence_chars': 0} for n in (8000, 32000, 100000)}
        with tempfile.TemporaryDirectory(prefix='reader-sol-evidence-') as d:
            d = pathlib.Path(d)
            source = d / 'source.json'
            for case in range(60):
                position = rng.randrange(90)
                chunks = [('Background administrative information. ' * 30)[:1050] for _ in range(90)]
                chunks[position] = ('The completion token for target is FACT_' + str(case) + '. This statement is the definitive answer. ' + 'Additional context. ' * 80)[:1050]
                text = '\n\n'.join(chunks)
                source.write_text(json.dumps({'url': 'https://example.test/source', 'main': text}))
                for budget in (8000, 32000, 100000):
                    args = worker.parse_args(['--source-artifact', str(source), '--out', str(d/'report.json'), '--question', 'What is the completion token for target?', '--answer-model', 'oracle', '--evidence-max-chars', str(budget)])
                    def oracle(endpoint, body, key, timeout):
                        evidence = json.loads(body['messages'][-1]['content'])['evidence']
                        match = re.search(r'FACT_\d+', evidence)
                        totals[str(budget)]['hits'] += bool(match)
                        totals[str(budget)]['evidence_chars'] += len(evidence)
                        return {'choices': [{'message': {'content': match.group() if match else 'Insufficient evidence.'}, 'finish_reason': 'stop'}]}, {}
                    with patch.object(worker, 'timed_http', side_effect=oracle):
                        worker.run(args)
            for budget, result in totals.items():
                result['mean_evidence_chars'] = round(result.pop('evidence_chars') / result['cases'], 2)
            METRICS['prefix_location_oracle'] = totals
            self.assertEqual(totals['100000']['hits'], 60)
            self.assertLess(totals['32000']['hits'], 60)

    def test_candidate_recall_and_short_facts(self):
        rng = random.Random(3)
        results = {'exact_query_hits': 0, 'semantic_query_hits': 0, 'cases': 200}
        for case in range(200):
            position = rng.randrange(40)
            chunks = ['What completion token belongs to target? This administrative placeholder has no answer. ' * 2 for _ in range(40)]
            chunks[position] = 'The completion token for target is FACT_' + str(case) + '. The definitive identifier was verified in the registry.'
            text = '\n\n'.join(chunks)
            exact = worker.passages(text, 'The definitive identifier verified registry', limit=8)
            semantic = worker.passages(text, 'What completion token belongs to target?', limit=8)
            results['exact_query_hits'] += any('FACT_' in c for c in exact.values())
            results['semantic_query_hits'] += any('FACT_' in c for c in semantic.values())
        short = 'Completion token: FACT_SHORT.\n\n' + 'Administrative background with more than sixty characters but no answer to the question.'
        results['short_fact_in_candidates'] = any('FACT_SHORT' in c for c in worker.passages(short, 'Completion token'))
        METRICS['candidate_recall'] = results
        self.assertEqual(results['exact_query_hits'], 200)
        self.assertFalse(results['short_fact_in_candidates'])

    def test_multi_fact_and_none(self):
        with tempfile.TemporaryDirectory(prefix='reader-sol-multifact-') as d:
            d = pathlib.Path(d)
            source = d / 'source.json'
            source.write_text(json.dumps({'url': 'https://example.test/source', 'main': 'Project code alpha has FACT_A first approval; the following background explains project code alpha.\n\nProject code alpha has FACT_B final approval; this is independent evidence for the second required fact.'}))
            base = ['--source-artifact', str(source), '--out', str(d/'report.json'), '--question', 'What are both approvals for project code alpha?', '--answer-model', 'oracle', '--classifier-key-env', '']
            observed = {}
            def oracle(endpoint, body, key, timeout):
                if 'questions' in body:
                    return {'answers': {'passage': {'choice': 'p0'}}}, {}
                evidence = json.loads(body['messages'][-1]['content'])['evidence']
                observed['facts'] = sorted(set(re.findall('FACT_[AB]', evidence)))
                return {'choices': [{'message': {'content': ' '.join(observed['facts'])}, 'finish_reason': 'stop'}]}, {}
            for mode in ('off', 'select'):
                with patch.object(worker, 'timed_http', side_effect=oracle):
                    worker.run(worker.parse_args(base + ['--classifier-mode', mode]))
                METRICS['multi_fact_' + mode] = observed['facts']
            self.assertEqual(METRICS['multi_fact_off'], ['FACT_A', 'FACT_B'])
            self.assertEqual(METRICS['multi_fact_select'], ['FACT_A'])
            with patch.object(worker, 'timed_http', return_value=({'answers': {'passage': {'choice': 'none'}}}, {})) as call:
                result = worker.run(worker.parse_args(base + ['--classifier-mode', 'select']))
            self.assertEqual(call.call_count, 1)
            self.assertEqual(result['answer'], 'The supplied page does not provide sufficient unambiguous evidence.')
            METRICS['none_skips_answer_call'] = True

    def test_parent_completeness_and_unicode_bounds(self):
        report = {'parent_result': {'answer': '🙂' * 2000, 'source': 'https://example.test/source'}, 'source_chars': 100000, 'source_total_chars': 500000, 'source_truncated': True, 'evidence_chars': 32000, 'evidence_narrowed': True}
        packet = adapter.bounded_result(report, 'id', 1)
        output = io.StringIO()
        server = adapter.Server(fixtures.argparse_namespace(), output)
        server.result(2, {'content': [{'type': 'text', 'text': json.dumps(packet, ensure_ascii=False)}]})
        METRICS['parent_packet'] = {'wire_bytes': len(output.getvalue().encode()), 'answer_chars': 2000, 'trace_fields': sorted(packet['trace']), 'completeness_fields_forwarded': any(k in packet['trace'] for k in ('source_total_chars', 'source_truncated', 'evidence_narrowed'))}
        self.assertFalse(METRICS['parent_packet']['completeness_fields_forwarded'])

    def test_cancel_retains_capacity_and_eof_waits(self):
        with tempfile.TemporaryDirectory(prefix='reader-sol-cancel-') as d:
            client = fixtures.Client(d, timeout=2, concurrent=1)
            try:
                client.initialize()
                client.call('sleep:.7', 2)
                client.wait_started()
                cancelled_at = time.monotonic()
                client.send('notifications/cancelled', {'requestId': 2})
                client.call('question', 3)
                immediate = client.receive()
                self.assertTrue(immediate['result']['isError'])
                client.close()
                METRICS['cancel_lifecycle'] = {'cancel_to_eof_exit_ms': round((time.monotonic()-cancelled_at)*1000, 2), 'capacity_rejected_after_cancel': True, 'cancelled_reply_count': client.messages.qsize(), 'cleanup_files': len(list(client.artifacts.glob('*/cleanup')))}
                self.assertEqual(client.messages.qsize(), 0)
            finally:
                if client.process.poll() is None:
                    client.close()

    def test_randomized_concurrency_cancel_correlation(self):
        rng = random.Random(42)
        cases = 15
        total_cancelled = 0
        for case in range(cases):
            with tempfile.TemporaryDirectory(prefix='reader-sol-races-') as d:
                client = fixtures.Client(d, concurrent=2)
                try:
                    client.initialize()
                    client.call('sleep:' + str(rng.uniform(.10, .18)), 10)
                    client.call('sleep:' + str(rng.uniform(.10, .18)), 11)
                    client.wait_started()
                    cancelled = rng.choice((10, 11))
                    client.send('notifications/cancelled', {'requestId': cancelled})
                    client.send('ping', request_id=20)
                    responses = [client.receive(), client.receive()]
                    self.assertEqual({r['id'] for r in responses}, {20, 21-cancelled})
                    with self.assertRaises(queue.Empty):
                        client.receive(timeout=.02)
                    total_cancelled += 1
                finally:
                    client.close()
        METRICS['randomized_lifecycle'] = {'cases': cases, 'suppressed_cancelled': total_cancelled, 'correlation_failures': 0}

    def test_collection_source_cap_and_cleanup(self):
        calls = []
        responses = [{'ok': True, 'identity': {'headless': True}, 'version': 'fixture'}, {'tab': {'id': 'local-fixture'}, 'ready': True, 'http_status': 200}, {'url': 'https://example.test/source', 'main': 'x'*100000, 'main_total_chars': 500000}, {'ok': True}]
        def fake_run(command, **kwargs):
            import subprocess
            calls.append(command)
            return subprocess.CompletedProcess(command, 0, json.dumps(responses.pop(0)), '')
        with patch.object(worker.subprocess, 'run', side_effect=fake_run):
            page = worker.collect('https://example.test/source', 'http://fixture.test', 'brw')
        METRICS['collection'] = {'read_command': calls[2], 'collected_chars': len(page['main']), 'total_chars': page['main_total_chars'], 'closed_tab': calls[3][-1]}
        self.assertIn('100000', calls[2])
        self.assertEqual(calls[3][-1], 'local-fixture')

if __name__ == '__main__':
    suite = unittest.defaultTestLoader.loadTestsFromTestCase(Probe)
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    OUT.write_text(json.dumps(METRICS, indent=2) + '\n')
    print(json.dumps(METRICS, indent=2))
    sys.exit(not result.wasSuccessful())
