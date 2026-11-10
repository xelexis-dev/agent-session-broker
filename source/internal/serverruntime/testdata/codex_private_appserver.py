"""Owned private app-server/TUI feasibility probe. Fixed-field output only.

No daemon commands, account auth, shared sockets, or real broker sessions.
Run with python3; all state is disposable under /private/tmp/pa-*.
"""
import base64
import fcntl
import hashlib
import json
import os
from pathlib import Path
import pty
import re
import select
import shlex
import shutil
import signal
import socket
import stat
import struct
import subprocess
import sys
import tempfile
import termios
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SELF = Path(__file__).resolve()
shared = {'__file__': str(SELF.with_name('codex_steer.py'))}
exec(compile(SELF.with_name('codex_steer.py').read_text().split('if len(sys.argv)>1')[0], str(SELF), 'exec'), shared)
WS = shared['WS']


def emit(label, **fields):
    print(json.dumps(dict(label=label, **fields), sort_keys=True), flush=True)


def event(root, kind, **extra):
    fields = {'kind': kind, 'pid': os.getpid(), 'ppid': os.getppid()}
    for key in ('XB_PROBE_MARK', 'CLAUDE_PROJECT_DIR'):
        value = os.environ.get(key, '')
        fields[key] = {'present': bool(value), 'sha256': hashlib.sha256(value.encode()).hexdigest()}
    with (root/'events.jsonl').open('a') as stream:
        stream.write(json.dumps(dict(fields, **extra))+'\n')


def rpc(ws, method, params):
    reply = ws.call(method, params)
    if 'error' in reply:
        raise RuntimeError(method+':'+str(reply['error'].get('code')))
    return reply['result']


def wait(label, predicate, seconds=20):
    end = time.monotonic()+seconds
    while time.monotonic() < end:
        value = predicate()
        if value:
            return value
        time.sleep(.03)
    raise RuntimeError(label)


def child_mode():
    if len(sys.argv) < 2:
        return False
    mode = sys.argv[1]
    if mode == 'hook':
        root = Path(sys.argv[2])
        data = json.load(sys.stdin)
        event(root, 'hook', hook=data.get('hook_event_name') if data.get('hook_event_name') in ('SessionStart', 'UserPromptSubmit', 'PreToolUse', 'PostToolUse', 'Stop') else 'other')
        print('{}', flush=True)
    elif mode == 'mcp':
        root = Path(sys.argv[2])
        event(root, 'mcp-start')
        for line in sys.stdin:
            q = json.loads(line)
            method = q.get('method')
            result = {}
            if method == 'initialize':
                result = {'protocolVersion': q['params']['protocolVersion'], 'capabilities': {'tools': {}}, 'serverInfo': {'name': 'private-probe', 'version': '1'}}
            elif method == 'tools/list':
                result = {'tools': [{'name': 'hold', 'description': 'Owned slow probe.', 'inputSchema': {'type': 'object', 'properties': {}}}]}
            elif method == 'tools/call':
                event(root, 'tool-start', at=time.monotonic())
                time.sleep(8)
                event(root, 'tool-done', at=time.monotonic())
                result = {'content': [{'type': 'text', 'text': 'owned fixture complete'}]}
            if 'id' in q:
                print(json.dumps({'jsonrpc': '2.0', 'id': q['id'], 'result': result}), flush=True)
    elif mode == 'steer':
        ws = WS(sys.argv[2])
        thread, turn, marker = sys.argv[3:6]
        queue = rpc(ws, 'thread/queue/list', {'threadId': thread, 'limit': 100})
        start = time.monotonic()
        reply = ws.call('turn/steer', {'threadId': thread, 'expectedTurnId': turn,
                       'clientUserMessageId': 'pa:'+marker, 'input': [{'type': 'text', 'text': marker, 'text_elements': []}]})
        emit('steer-child', queueAvailable=isinstance(queue.get('data'), list), accepted='result' in reply,
             sameTurn=reply.get('result', {}).get('turnId') == turn, latencyMs=round((time.monotonic()-start)*1000, 3),
             errorCode=reply.get('error', {}).get('code'))
        ws.close()
    else:
        return False
    return True


class TUI:
    def __init__(self, env, project, endpoint, thread=None, flags=()):
        self.buffer = bytearray()
        self.stop = threading.Event()
        self.master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 42, 160, 0, 0))
        args = ['codex']+(['resume', thread] if thread else [])+['--remote', 'unix://'+str(endpoint), '--no-alt-screen', *flags]
        self.p = subprocess.Popen(args, env=env, cwd=project, stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
        os.close(slave)
        tuis.append(self)
        def drain():
            trusted = False
            while not self.stop.is_set():
                try:
                    if not select.select([self.master], [], [], .1)[0]:
                        continue
                    data = os.read(self.master, 65536)
                    if not data:
                        return
                    if b'\x1b[6n' in data:
                        os.write(self.master, b'\x1b[1;1R')
                    self.buffer.extend(data)
                    del self.buffer[:-100000]
                    text = self.text().lower()
                    if not trusted and ('hooks need review' in text or 'hooks are new or changed' in text):
                        trusted = True
                        emit('hook-trust-panel', choices=[re.sub(r'/private/tmp/[^ \n]+', '<OWNED>', x) for x in re.findall(r'.{0,10}(?:Trust|Review|Continue|permission|1\.|2\.|3\.).{0,160}', self.text())[-10:]])
                        os.write(self.master, b'\x1b[B')
                        time.sleep(.4)
                        os.write(self.master, b'\r')
                except OSError:
                    return
        threading.Thread(target=drain, daemon=True).start()

    def text(self):
        return re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]', '', self.buffer.decode(errors='ignore'))

    def type(self, text):
        os.write(self.master, text.encode())
        time.sleep(.15)
        os.write(self.master, b'\r')

    def close(self):
        natural = self.p.poll() is not None
        if not natural:
            os.write(self.master, b'\x04')
            try:
                self.p.wait(timeout=3)
                natural = True
            except subprocess.TimeoutExpired:
                os.write(self.master, b'\x03')
                time.sleep(.15)
                os.write(self.master, b'\x03')
                try:
                    self.p.wait(timeout=3)
                    natural = True
                except subprocess.TimeoutExpired:
                    os.killpg(self.p.pid, signal.SIGTERM)
                    self.p.wait(timeout=5)
        self.stop.set()
        try:
            os.close(self.master)
        except OSError:
            pass
        return natural


class Model(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        data = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        thread = self.headers.get('thread-id') or data.get('prompt_cache_key')
        raw = json.dumps(data.get('input', []))
        current = modes.get(thread, {})
        marker = current.get('marker', '')
        marker_present = bool(marker and marker in raw)
        main = bool(data.get('tools'))
        calls.append({'thread': thread, 'model': data.get('model'), 'effort': data.get('reasoning', {}).get('effort'),
                      'main': main, 'marker': marker_present, 'at': time.monotonic()})
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.end_headers()
        def send(name, **fields):
            self.wfile.write(('event: '+name+'\ndata: '+json.dumps(dict(type=name, **fields))+'\n\n').encode())
            self.wfile.flush()
        try:
            send('response.created', response={'id': 'resp_private', 'object': 'response', 'status': 'in_progress', 'output': []})
            if main and current.get('mode') == 'tool' and not current.get('issued'):
                current['issued'] = True
                entries = []
                for tool in data.get('tools', []):
                    if tool.get('type') == 'namespace':
                        entries.extend((t, tool['name']) for t in tool.get('tools', []))
                    else:
                        entries.append((tool, None))
                tool, ns = next((t, n) for t, n in entries if 'hold' in t.get('name', ''))
                item = {'type': 'function_call', 'id': 'fc_private', 'call_id': 'private_hold', 'name': tool['name'], 'arguments': '{}'}
                if ns:
                    item['namespace'] = ns
                send('response.output_item.added', output_index=0, item=item)
                send('response.output_item.done', output_index=0, item=item)
            else:
                item = {'id': 'msg_private', 'type': 'message', 'role': 'assistant', 'status': 'in_progress',
                        'content': [{'type': 'output_text', 'text': 'private fixture complete', 'annotations': []}]}
                send('response.output_item.added', output_index=0, item=item)
                if main and current.get('mode') == 'stream' and not marker_present:
                    current['streaming'] = True
                    end = time.monotonic()+15
                    while time.monotonic() < end and not current.get('release'):
                        send('response.output_text.delta', item_id=item['id'], output_index=0, content_index=0, delta='progress ')
                        time.sleep(.1)
                item['status'] = 'completed'
                send('response.output_text.delta', item_id=item['id'], output_index=0, content_index=0, delta='private fixture complete')
                send('response.output_item.done', output_index=0, item=item)
            send('response.completed', response={'id': 'resp_private', 'object': 'response', 'status': 'completed', 'output': [item],
                 'usage': {'input_tokens': 1, 'output_tokens': 1, 'total_tokens': 2}})
        except (BrokenPipeError, ConnectionResetError):
            pass
        except Exception as exc:
            emit('model-failure', kind=type(exc).__name__)


def start_server(env, project, path, model_name='gpt-6.1-sol', effort='low'):
    p = subprocess.Popen(['codex', 'app-server', '--listen', 'unix://'+str(path), '-c', 'model='+json.dumps(model_name),
                          '-c', 'model_reasoning_effort='+json.dumps(effort), '-c', 'approval_policy="never"',
                          '-c', 'sandbox_mode="danger-full-access"'], env=env, cwd=project,
                         stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, start_new_session=True)
    servers.append(p)
    errors = bytearray()
    def drain():
        for line in p.stderr:
            errors.extend(line)
            del errors[:-30000]
    threading.Thread(target=drain, daemon=True).start()
    wait('private-socket', lambda: path.exists() or p.poll() is not None)
    return p, errors


def stop_server(p):
    if p.poll() is None:
        os.killpg(p.pid, signal.SIGTERM)
        try:
            p.wait(timeout=5)
        except subprocess.TimeoutExpired:
            os.killpg(p.pid, signal.SIGKILL)
            p.wait(timeout=5)


def terminal(ws, thread):
    rows = rpc(ws, 'thread/turns/list', {'threadId': thread, 'limit': 100})['data']
    return rows and rows[0]['status'] != 'inProgress'


def events():
    path = base/'events.jsonl'
    return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []


def last_thread():
    return next((c['thread'] for c in reversed(calls) if c['main']), None)


def steer_case(ui, ws, endpoint, thread, mode):
    marker = 'PRIVATE_STEER_'+mode.upper()
    modes[thread] = {'mode': mode, 'marker': marker}
    before = len(events())
    ui.type('Exercise the private '+mode+' fixture.')
    if mode == 'stream':
        wait('stream-active', lambda: modes[thread].get('streaming'))
    else:
        wait('tool-active', lambda: any(e['kind'] == 'tool-start' for e in events()[before:]))
    turn = rpc(ws, 'thread/turns/list', {'threadId': thread, 'limit': 1})['data'][0]['id']
    child = subprocess.run([sys.executable, str(SELF), 'steer', str(endpoint), thread, turn, marker],
                           env=env_a, cwd=project_a, capture_output=True, text=True, timeout=25)
    assert child.returncode == 0, 'steer-child-failed'
    receipt = json.loads(child.stdout)
    wait('model-marker', lambda: any(c['thread'] == thread and c['marker'] for c in calls))
    modes[thread]['release'] = True
    wait('same-turn-terminal', lambda: terminal(ws, thread))
    page = rpc(ws, 'thread/items/list', {'threadId': thread, 'turnId': turn, 'limit': 100})
    matches = [x for x in page['data'] if x.get('item', {}).get('clientId') == 'pa:'+marker]
    rollout = any(marker in p.read_text() for p in ch.rglob('*.jsonl') if 'sessions' in p.parts)
    emit('M5-'+mode, **{k: receipt[k] for k in ('queueAvailable', 'accepted', 'sameTurn', 'latencyMs')},
         historyMatches=len(matches), originalTurn=all(x.get('turnId') == turn for x in matches), rolloutMarker=rollout,
         toolSurvived=any(e['kind'] == 'tool-done' for e in events()[before:]) if mode == 'tool' else None)
    assert receipt['accepted'] and receipt['sameTurn'] and len(matches) == 1 and rollout, 'same-turn-proof'


def main():
    global base, ch, env_a, project_a, calls, modes, servers, tuis
    base = Path(tempfile.mkdtemp(prefix='pa-', dir='/private/tmp'))
    home, ch, project_a, project_b = base/'h', base/'c', base/'a', base/'b'
    for path in (home, ch, project_a, project_b):
        path.mkdir(mode=0o700)
    servers, tuis, controllers, calls, modes = [], [], [], [], {}
    success = False
    model = ThreadingHTTPServer(('127.0.0.1', 0), Model)
    threading.Thread(target=model.serve_forever, daemon=True).start()
    env = {k: v for k, v in os.environ.items() if k in ('PATH', 'LANG', 'SHELL', 'SDKROOT')}
    env.update(HOME=str(home), CODEX_HOME=str(ch), TMPDIR=str(base), TERM='xterm-256color')
    env_a = dict(env, XB_PROBE_MARK='SERVER_A', CLAUDE_PROJECT_DIR=str(project_a)+'#A')
    env_b = dict(env, XB_PROBE_MARK='SERVER_B', CLAUDE_PROJECT_DIR=str(project_b)+'#B')
    tui_env = dict(env, XB_PROBE_MARK='TUI_ONLY', CLAUDE_PROJECT_DIR=str(project_a)+'#TUI')
    (ch/'config.toml').write_text('''model = "gpt-6.1-sol"
model_provider = "probe"
model_reasoning_effort = "medium"
[model_providers.probe]
name = "Local synthetic provider"
base_url = "http://127.0.0.1:%d/v1"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = false
[mcp_servers.private-probe]
command = %s
args = [%s,"mcp",%s]
[projects.%s]
trust_level = "trusted"
[projects.%s]
trust_level = "trusted"
''' % (model.server_port, json.dumps(sys.executable), json.dumps(str(SELF)), json.dumps(str(base)), json.dumps(str(project_a)), json.dumps(str(project_b))))
    for project in (project_a, project_b):
        subprocess.run(['git', 'init', '-q', str(project)], check=True)
        (project/'.codex').mkdir()
        hook = {'type': 'command', 'command': shlex.join([sys.executable, str(SELF), 'hook', str(base)]), 'timeout': 10}
        (project/'.codex/hooks.json').write_text(json.dumps({'hooks': {e: [{'hooks': [hook]}] for e in ('SessionStart', 'UserPromptSubmit', 'PreToolUse', 'PostToolUse', 'Stop')}}))
    try:
        version = subprocess.run(['codex', '--version'], env=env, cwd=base, capture_output=True, text=True, check=True).stdout.strip()
        emit('version', version=version)
        endpoint_a, endpoint_b = base/'a.sock', base/'b.sock'
        server_a, logs_a = start_server(env_a, project_a, endpoint_a)
        assert server_a.poll() is None, 'private-server-start'
        ws = WS(endpoint_a)
        controllers.append(ws)
        config = rpc(ws, 'config/read', {'includeLayers': False}).get('config', {})
        emit('M1', started=True, model=config.get('model'), effort=config.get('model_reasoning_effort'),
             pid=server_a.pid, socketMode=oct(stat.S_IMODE(endpoint_a.stat().st_mode)), socketOwned=endpoint_a.stat().st_uid == os.getuid())
        ui = TUI(tui_env, project_a, endpoint_a)
        wait('tui-ready', lambda: 'gpt-6.1-sol' in ui.text())
        ui.type('Initialize the private fixture.')
        thread = wait('tui-model', last_thread)
        wait('tui-initial-finish', lambda: terminal(ws, thread))
        ui.type('/status')
        time.sleep(.5)
        emit('M2-new', attached=ui.p.poll() is None, thread=thread, modelVisible='gpt-6.1-sol' in ui.text(), effortVisible=bool(re.search(r'\blow\b', ui.text(), re.I)))
        # Parser/attach compatibility, measured independently for new and resume.
        for resumed in (False, True):
            for name, flags in [('a', ['-a', 'never']), ('s', ['-s', 'danger-full-access']), ('m', ['-m', 'gpt-6.1-sol']), ('c-effort', ['-c', 'model_reasoning_effort="high"']), ('c-model', ['-c', 'model="gpt-6.1-sol"'])]:
                candidate = TUI(tui_env, project_a, endpoint_a, thread if resumed else None, flags)
                time.sleep(.6)
                text = candidate.text().lower()
                emit('M2-flags', resume=resumed, option=name, accepted=candidate.p.poll() is None,
                     unsupported=('not supported' in text or 'cannot be used' in text or 'unexpected argument' in text), exit=candidate.p.poll())
                candidate.close()
        # Reattach the measured TUI to its original thread after the flag trials.
        natural = ui.close()
        alive = server_a.poll() is None
        ui = TUI(tui_env, project_a, endpoint_a, thread)
        time.sleep(.7)
        count = len(calls)
        ui.type('Confirm the resumed private fixture.')
        wait('resume-model', lambda: len(calls) > count)
        wait('resume-finish', lambda: terminal(ws, thread))
        emit('M6-exit-reattach', naturalExit=natural, serverSurvived=alive, resumeAttached=ui.p.poll() is None)
        wait('hooks-mcp', lambda: any(e['kind'] == 'hook' for e in events()) and any(e['kind'] == 'mcp-start' for e in events()))
        for kind, label in [('hook', 'M3'), ('mcp-start', 'M4')]:
            rows = [e for e in events() if e['kind'] == kind]
            expected = hashlib.sha256(env_a['XB_PROBE_MARK'].encode()).hexdigest()
            emit(label, count=len(rows), serverEnv=all(e['XB_PROBE_MARK']['sha256'] == expected for e in rows),
                 markerHashes=sorted({e['XB_PROBE_MARK']['sha256'] for e in rows}), parentEnvPresent=all(e['CLAUDE_PROJECT_DIR']['present'] for e in rows))
        steer_case(ui, ws, endpoint_a, thread, 'stream')
        steer_case(ui, ws, endpoint_a, thread, 'tool')
        # Keep A generating while B turns on the exact same HOME/CODEX_HOME.
        modes[thread] = {'mode': 'stream', 'marker': 'M7_RELEASE'}
        ui.type('Run concurrent private fixture A.')
        wait('A-concurrent-active', lambda: modes[thread].get('streaming'))
        server_b, logs_b = start_server(env_b, project_b, endpoint_b, 'gpt-6.1-terra', 'high')
        ws_b = WS(endpoint_b)
        controllers.append(ws_b)
        ui_b = TUI(tui_env, project_b, endpoint_b)
        time.sleep(.8)
        count = len(calls)
        ui_b.type('Run concurrent private fixture B.')
        thread_b = wait('B-model', lambda: next((c['thread'] for c in calls[count:] if c['main'] and c['thread'] != thread), None))
        wait('B-completed', lambda: terminal(ws_b, thread_b))
        modes[thread]['release'] = True
        wait('A-completed', lambda: terminal(ws, thread))
        samples = {t: [(c['model'], c['effort']) for c in calls if c['thread'] == t and c['main']] for t in (thread, thread_b)}
        emit('M7', bothAlive=server_a.poll() is None and server_b.poll() is None, distinctThreads=thread != thread_b,
             aModels=sorted({str(x) for x in samples[thread]}), bModels=sorted({str(x) for x in samples[thread_b]}),
             bMcpEnv=any(e['kind'] == 'mcp-start' and e['XB_PROBE_MARK']['sha256'] == hashlib.sha256(b'SERVER_B').hexdigest() for e in events()))
        stop_server(server_a)
        time.sleep(2)
        text = ui.text().lower()
        emit('M6-server-stop', tuiExit=ui.p.poll(), tuiStillAlive=ui.p.poll() is None,
             failureVisible=any(word in text for word in ('connection closed', 'disconnected', 'reconnect', 'connection lost', 'failed to connect', 'transport error')))
        ui.close()
        natural_b = ui_b.close()
        stop_server(server_b)
        emit('M6-launcher-cleanup', naturalTuiExit=natural_b, serverStopped=server_b.poll() is not None)
        for length in (100, 103, 104, 105):
            path = base/('s'*(length-len(str(base))-1))
            p, errors = start_server(env_a, project_a, path)
            accepted = path.exists() and p.poll() is None
            emit('M8-length', bytes=len(os.fsencode(path)), accepted=accepted,
                 tooLong=b'too long' in errors.lower() or b'length' in errors.lower())
            stop_server(p)
        success = True
    except BaseException as exc:
        emit('failure', kind=type(exc).__name__, stage=str(exc) if isinstance(exc, (AssertionError, RuntimeError)) else 'fixture', tui=[{'exit':t.p.poll(),'flags':{word:word in t.text().lower() for word in ('press enter','trust','login','sign in','unsupported','error','failed','hooks need review','continue','remote','model','context left','invalid','refused','permission')}} for t in tuis])
    finally:
        for value in modes.values():
            value['release'] = True
        for controller in controllers:
            try:
                controller.close()
            except OSError:
                pass
        for tui in tuis:
            try:
                tui.close()
            except (OSError, subprocess.TimeoutExpired):
                pass
        for server in servers:
            stop_server(server)
        model.shutdown()
        model.server_close()
        def owned():
            rows = subprocess.run(['lsof', '-t', '+D', str(base)], capture_output=True, text=True).stdout.split()
            return {int(x) for x in rows}-{os.getpid()}
        for sig in (signal.SIGTERM, signal.SIGKILL):
            ids = owned()
            if not ids:
                break
            for pid in ids:
                try:
                    os.kill(pid, sig)
                except ProcessLookupError:
                    pass
            time.sleep(.5)
        remaining = owned()
        if remaining:
            subprocess.run(['ps', '-o', 'pid,ppid,rss', '-p', ','.join(map(str, remaining))])
        emit('cleanup', remaining=len(remaining))
        if not remaining:
            shutil.rmtree(base)
    return 0 if success and not remaining else 1


if __name__ == '__main__':
    if not child_mode():
        sys.exit(main())
