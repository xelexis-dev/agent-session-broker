"""L4 host-only race measurement. No broker, credentials or paid model calls.

Run: python3 codex_steer_carryover.py [--n 20]
Only fixed IDs, counts, statuses and timing labels reach stdout. Raw host state
and fake model bodies stay in memory or in the disposable isolated profile.
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Reuse the exact WS framing/initialization prefix, without running its broker harness.
fixture = Path(__file__).with_name('codex_steer.py')
shared = {'__file__': str(fixture)}
exec(compile(fixture.read_text().split("if len(sys.argv)>1")[0], str(fixture), 'exec'), shared)
WS = shared['WS']


def emit(**fields):
    print(json.dumps(fields, sort_keys=True), flush=True)


def wait(label, predicate, seconds=20):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        value = predicate()
        if value:
            return value
        time.sleep(.005)
    raise RuntimeError(label)


def rpc(ws, method, params):
    reply = ws.call(method, params)
    if 'error' in reply:
        # Error messages may echo input; keep only numeric codes and method.
        raise RuntimeError(method + ':' + str(reply['error'].get('code')))
    return reply['result']


class Model(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        self.rfile.read(int(self.headers['Content-Length']))
        is_target = state.get('armed', False)
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.end_headers()

        def send(name, **fields):
            self.wfile.write(('event: ' + name + '\ndata: ' + json.dumps(dict(type=name, **fields)) + '\n\n').encode())
            self.wfile.flush()

        item = {'id': 'msg_fixture', 'type': 'message', 'role': 'assistant', 'status': 'in_progress',
                'content': [{'type': 'output_text', 'text': 'fixture complete', 'annotations': []}]}
        try:
            send('response.created', response={'id': 'resp_fixture', 'object': 'response', 'status': 'in_progress', 'output': []})
            send('response.output_item.added', output_index=0, item=item)
            send('response.output_text.delta', item_id=item['id'], output_index=0, content_index=0, delta='fixture ')
            if is_target:
                state['streaming'].set()
                if not state['release'].wait(20):
                    raise RuntimeError('model-release-timeout')
            item['status'] = 'completed'
            send('response.output_text.delta', item_id=item['id'], output_index=0, content_index=0, delta='complete')
            send('response.output_item.done', output_index=0, item=item)
            send('response.completed', response={'id': 'resp_fixture', 'object': 'response', 'status': 'completed',
                 'output': [item], 'usage': {'input_tokens': 1, 'output_tokens': 1, 'total_tokens': 2}})
        except (BrokenPipeError, ConnectionResetError):
            pass


def pages(ws, method, thread):
    entries, cursor, seen = [], None, set()
    while True:
        params = {'threadId': thread, 'limit': 2}
        if method == 'thread/items/list':
            params['sortDirection'] = 'desc'
        if cursor is not None:
            params['cursor'] = cursor
        result = rpc(ws, method, params)
        assert isinstance(result.get('data'), list), 'invalid-page-data'
        assert 'nextCursor' in result, 'missing-page-frontier'
        entries.extend(result['data'])
        cursor = result['nextCursor']
        if cursor is None:
            return entries
        assert isinstance(cursor, str) and cursor and cursor not in seen, 'invalid-page-cursor'
        seen.add(cursor)


def locate(ws, thread, target, marker):
    history = pages(ws, 'thread/items/list', thread)
    queue = pages(ws, 'thread/queue/list', thread)
    matches = [entry for entry in history if entry.get('item', {}).get('clientId') == marker]
    return {'target': sum(entry.get('turnId') == target for entry in matches),
            'next': sum(entry.get('turnId') != target for entry in matches),
            'queue': sum(entry.get('clientUserMessageId') == marker for entry in queue)}


def terminal(ws, thread, turn):
    turns = rpc(ws, 'thread/turns/list', {'threadId': thread, 'limit': 100})['data']
    return next((t['status'] for t in turns if t['id'] == turn and t['status'] != 'inProgress'), None)


def start(ws, thread):
    return rpc(ws, 'turn/start', {'threadId': thread, 'input': [{'type': 'text', 'text': 'Complete the fixture.', 'text_elements': []}]})['turn']['id']


def run_case(ws, observer, case, index):
    marker = 'l4:' + case + ':' + str(index)
    thread = rpc(ws, 'thread/start', {'cwd': str(project), 'model': 'gate', 'approvalPolicy': 'never', 'sandbox': 'danger-full-access'})['thread']['id']
    state.clear()
    state.update(marker=marker, armed=True, streaming=threading.Event(), release=threading.Event())
    turn = start(ws, thread)
    assert state['streaming'].wait(20), 'streaming-timeout'
    # A separate reader establishes server terminal state without letting the
    # steering connection consume its queued turn/completed notification.
    params = {'threadId': thread, 'expectedTurnId': turn, 'clientUserMessageId': marker,
              'input': [{'type': 'text', 'text': 'Measured synthetic input.', 'text_elements': []}]}
    ws.notifications.clear()
    if case == 'after-completed-unseen':
        state['release'].set()
        wait('server-terminal', lambda: terminal(observer, thread, turn))
        assert not any(n.get('method') == 'turn/completed' for n in ws.notifications), 'completion-already-consumed'
        reply = ws.call('turn/steer', params)
    elif case == 'interrupt':
        # Alternate which request gets the head start; both travel on independent sockets.
        box = {}
        def interrupt():
            box['reply'] = observer.call('turn/interrupt', {'threadId': thread, 'turnId': turn})
        worker = threading.Thread(target=interrupt)
        if index % 2 == 0:
            worker.start()
            reply = ws.call('turn/steer', params)
        else:
            ws.seq += 1
            seq = ws.seq
            ws.send({'jsonrpc': '2.0', 'id': seq, 'method': 'turn/steer', 'params': params})
            worker.start()
            while True:
                reply = ws.read(time.monotonic() + 20)
                if reply.get('id') == seq and ('result' in reply or 'error' in reply):
                    break
        worker.join(20)
        assert not worker.is_alive() and 'reply' in box, 'interrupt-timeout'
        assert 'result' in box['reply'], 'interrupt-rejected'
        state['release'].set()
    else:
        # Vary the lead around final SSE completion, including steer-before-release.
        delay = (-.005, 0, .0005, .002, .005)[index % 5]
        if delay < 0:
            reply = ws.call('turn/steer', params)
            state['release'].set()
        else:
            state['release'].set()
            if delay:
                time.sleep(delay)
            reply = ws.call('turn/steer', params)
    state['armed'] = False
    status = wait('target-terminal', lambda: terminal(ws, thread, turn))
    before = locate(ws, thread, turn, marker)
    follow = start(ws, thread)
    wait('followup-terminal', lambda: terminal(ws, thread, follow))
    after = locate(ws, thread, turn, marker)
    time.sleep(.03)
    late = locate(ws, thread, turn, marker)
    assert after == late, 'late-materialization'
    assert sum(late.values()) <= 1, 'duplicate-original-client-id'
    emit(case=case, index=index, clientUserMessageId=marker, accepted='result' in reply,
         errorCode=reply.get('error', {}).get('code'), targetStatus=status, before=before, after=after,
         nowhere=sum(after.values()) == 0)
    return {'accepted': int('result' in reply), **{k: int(v > 0) for k, v in after.items()},
            'nowhere': int(sum(after.values()) == 0)}


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--n', type=int, default=20)
    parser.add_argument('--daemon-binary', default='codex')
    args = parser.parse_args()
    assert args.n > 0
    base = Path(tempfile.mkdtemp(prefix='l4-', dir='/private/tmp'))
    home, ch, project = base/'h', base/'c', base/'p'
    for path in (home, ch, project):
        path.mkdir()
    env = {k: v for k, v in os.environ.items() if k in ('PATH', 'LANG', 'SDKROOT', 'SHELL')}
    env.update(HOME=str(home), CODEX_HOME=str(ch), TMPDIR=str(base))
    state = {}
    model = ThreadingHTTPServer(('127.0.0.1', 0), Model)
    threading.Thread(target=model.serve_forever, daemon=True).start()
    (ch/'config.toml').write_text('''model = "gate"
model_provider = "gate"
[model_providers.gate]
name = "Local fake model"
base_url = "http://127.0.0.1:%d/v1"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = false
''' % model.server_port)
    controllers = []
    success = False
    try:
        subprocess.run([args.daemon_binary, 'app-server', 'daemon', 'start'], env=env, cwd=project,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True, timeout=30)
        endpoint = ch/'app-server-control/app-server-control.sock'
        wait('owned-socket', endpoint.exists)
        ws, observer = WS(endpoint), WS(endpoint)
        controllers.extend([ws, observer])
        version = subprocess.run(['codex', 'app-server', 'daemon', 'version'], env=env, cwd=project,
                                 capture_output=True, text=True, check=True).stdout
        version = json.loads(version)
        emit(label='versions', **{k: version[k] for k in ('cliVersion', 'appServerVersion', 'managedCodexVersion', 'status')})
        totals = {}
        for case in ('completion', 'after-completed-unseen', 'interrupt'):
            total = dict(accepted=0, target=0, next=0, queue=0, nowhere=0)
            for i in range(args.n):
                result = run_case(ws, observer, case, i)
                for key, value in result.items():
                    total[key] += value
            totals[case] = total
        emit(label='totals', n=args.n, cases=totals)
        success = True
    except BaseException as exc:
        emit(label='failure', kind=type(exc).__name__, stage=str(exc) if isinstance(exc, (RuntimeError, AssertionError)) else 'fixture')
    finally:
        if state.get('release'):
            state['release'].set()
        for controller in controllers:
            controller.close()
        subprocess.run(['codex', 'app-server', 'daemon', 'stop'], env=env, cwd=project,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=20)
        model.shutdown()
        model.server_close()
        def owned_pids():
            rows = subprocess.run(['lsof', '-t', '+D', str(base)], capture_output=True, text=True).stdout.split()
            return {int(pid) for pid in rows} - {os.getpid()}
        # daemon stop may leave its owned shell snapshot helper running briefly.
        for sig in (None, signal.SIGTERM, signal.SIGKILL):
            remaining = owned_pids()
            if not remaining:
                break
            if sig is not None:
                for pid in remaining:
                    try:
                        os.kill(pid, sig)
                    except ProcessLookupError:
                        pass
            time.sleep(.5)
        remaining = owned_pids()
        emit(label='cleanup', remaining=len(remaining))
        if not remaining:
            shutil.rmtree(base)
    raise SystemExit(0 if success and not remaining else 1)
