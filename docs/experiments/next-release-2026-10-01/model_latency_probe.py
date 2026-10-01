import importlib.util
import json
import pathlib
import statistics
import sys
import time
import urllib.request

sys.dont_write_bytecode = True
path = pathlib.Path(__file__).resolve().parents[3] / 'scripts/measure-local-browser-model.py'
spec = importlib.util.spec_from_file_location('canonical', path)
canonical = importlib.util.module_from_spec(spec)
spec.loader.exec_module(canonical)
rows = []

def structured(case):
    result = []
    for line in case['observation']:
        parts = line.split()
        flags = {'hidden', 'visible', 'disabled', 'enabled', 'stale', 'editable'}
        boundary = next(i for i, part in enumerate(parts) if part in flags)
        result.append({'ref': parts[0], 'role': parts[1], 'name': ' '.join(parts[2:boundary]), 'visible': 'visible' in parts, 'disabled': 'disabled' in parts, 'stale': 'stale' in parts, 'editable': 'editable' in parts, 'value': line.split('value=', 1)[1] if 'value=' in line else ''})
    return result

for pair, case in enumerate(canonical.cases()[:3]):
    observation = structured(case)
    text = ('Archived public catalog entries describe unrelated geology, pottery, gardening, and lighthouse maintenance. ' * 20)[:len(json.dumps(observation))]
    order = ('compact', 'expanded') if pair % 2 == 0 else ('expanded', 'compact')
    for mode in order:
        content = {'goal': case['goal'], 'observation': observation}
        if mode == 'expanded':
            content['irrelevant_page_context'] = [text] * 20
        body = {'model': 'qwen3.5-4b-mlx', 'messages': [{'role': 'system', 'content': canonical.SYSTEM}, {'role': 'user', 'content': json.dumps(content)}], 'tools': canonical.TOOLS, 'tool_choice': 'auto', 'temperature': 0, 'max_tokens': 128, 'stream': True, 'stream_options': {'include_usage': True}, 'reasoning_effort': 'none', 'chat_template_kwargs': {'enable_thinking': False}}
        payload = json.dumps(body).encode()
        row = {'pair': pair, 'case': case['name'], 'mode': mode, 'request_bytes': len(payload), 'message_bytes': len(body['messages'][-1]['content'].encode()), 'first_visible_delta_ms': None, 'pass': False, 'usage': None}
        start = time.perf_counter()
        tools = {}
        prose = ''
        raw_bytes = 0
        try:
            request = urllib.request.Request('http://127.0.0.1:1234/v1/chat/completions', data=payload, headers={'Content-Type': 'application/json'})
            with urllib.request.urlopen(request, timeout=20) as response:
                row['headers_ms'] = round((time.perf_counter()-start)*1000, 3)
                for raw in response:
                    raw_bytes += len(raw)
                    if raw_bytes > 1048576 or time.perf_counter()-start > 20:
                        raise TimeoutError('Bounded response size/time reached')
                    if not raw.startswith(b'data:'):
                        continue
                    raw = raw[5:].strip()
                    if raw == b'[DONE]':
                        break
                    event = json.loads(raw)
                    row['returned_model'] = event.get('model')
                    if event.get('usage'):
                        row['usage'] = event['usage']
                    for choice in event.get('choices', []):
                        delta = choice.get('delta', {})
                        if row['first_visible_delta_ms'] is None and (delta.get('content') or any(x.get('function', {}).get('name') or x.get('function', {}).get('arguments') for x in delta.get('tool_calls', []))):
                            row['first_visible_delta_ms'] = round((time.perf_counter()-start)*1000, 3)
                        prose += delta.get('content') or ''
                        for call in delta.get('tool_calls', []):
                            item = tools.setdefault(call['index'], {'name': '', 'arguments': ''})
                            item['name'] += call.get('function', {}).get('name') or ''
                            item['arguments'] += call.get('function', {}).get('arguments') or ''
                        if choice.get('finish_reason'):
                            row['finish_reason'] = choice['finish_reason']
            row['actual'] = [{'name': tools[i]['name'], 'arguments': json.loads(tools[i]['arguments'])} for i in sorted(tools)]
            row['prose'] = prose
            row['pass'] = row['actual'] == [case['expected']]
        except Exception as error:
            row['error'] = str(error)[:500]
        row['complete_ms'] = round((time.perf_counter()-start)*1000, 3)
        row['response_bytes'] = raw_bytes
        rows.append(row)
        print(json.dumps(row), flush=True)

summary = {}
for mode in ('compact', 'expanded'):
    cohort = [r for r in rows if r['mode'] == mode]
    summary[mode] = {'calls': len(cohort), 'correct': sum(r['pass'] for r in cohort), 'median_complete_ms': statistics.median(r['complete_ms'] for r in cohort), 'median_first_visible_delta_ms': statistics.median(r['first_visible_delta_ms'] for r in cohort if r['first_visible_delta_ms'] is not None) if any(r['first_visible_delta_ms'] is not None for r in cohort) else None, 'request_bytes': [r['request_bytes'] for r in cohort], 'prompt_tokens': [(r['usage'] or {}).get('prompt_tokens') for r in cohort]}
output = {'scope': 'Six serial synthetic tool choices, three rotated compact/expanded pairs. No proposed browser actions executed. Concurrent parent browser benchmarking may create machine contention. Streaming first-visible-delta is not server first-token/prefill/queue timing. Local loaded model unchanged.', 'results': rows, 'summary': summary}
pathlib.Path('/tmp/brw-next-model-latency.json').write_text(json.dumps(output, indent=2) + '\n')
print(json.dumps(summary, indent=2))
