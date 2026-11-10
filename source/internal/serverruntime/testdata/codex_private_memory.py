"""M10 early gate: owned PID/RSS only; no daemon or plugin commands.

Compare completed embedded conversation(s) with private server(s) and attached
remote TUI(s), even if the latter remain at a startup UI. All state is temporary.
"""
import json
import os
from pathlib import Path
import shutil
import shlex
import signal
import statistics
import subprocess
import sys
import tempfile
import threading
import time
from http.server import ThreadingHTTPServer

import codex_private_appserver as probe


class PaneProcess:
    def __init__(self, pid):
        self.pid = pid
    def poll(self):
        try:
            os.kill(self.pid, 0)
            return None
        except ProcessLookupError:
            return 0


class MemoryTUI:
    def __init__(self, env, project, endpoint):
        self.socket = Path(env['TMPDIR'])/('tmux-'+str(len(probe.tuis))+'.sock')
        self.prefix = ['tmux', '-S', str(self.socket), '-f', '/dev/null']
        args = ['codex', '--no-alt-screen'] + (['--remote', 'unix://'+str(endpoint)] if endpoint else ['--no-daemon'])
        subprocess.run([*self.prefix, 'new-session', '-d', '-s', 'probe', '-x', '160', '-y', '42', '-c', str(project),
                        'exec '+shlex.join(args)], env=env, check=True, capture_output=True)
        result = subprocess.run([*self.prefix, 'display-message', '-p', '#{pane_pid}'], capture_output=True, text=True, check=True)
        self.p = PaneProcess(int(result.stdout.strip()))
        probe.tuis.append(self)
    def text(self):
        result = subprocess.run([*self.prefix, 'capture-pane', '-p', '-S', '-100'], capture_output=True, text=True)
        return result.stdout
    def type(self, text):
        subprocess.run([*self.prefix, 'send-keys', '-l', text], check=True, capture_output=True)
        subprocess.run([*self.prefix, 'send-keys', 'Enter'], check=True, capture_output=True)
    def close(self):
        subprocess.run([*self.prefix, 'kill-server'], capture_output=True)


def descendants(root):
    found, pending = {root}, [root]
    while pending:
        parent = pending.pop()
        result = subprocess.run(['pgrep', '-P', str(parent)], capture_output=True, text=True)
        for value in result.stdout.split():
            pid = int(value)
            if pid not in found:
                found.add(pid)
                pending.append(pid)
    return found


def rss(roots):
    pids = set().union(*(descendants(p) for p in roots))
    result = subprocess.run(['ps', '-o', 'pid=,rss=', '-p', ','.join(map(str, sorted(pids)))],
                            capture_output=True, text=True, check=True)
    rows = [{'pid': int(p), 'rssKiB': int(k)} for p, k in (line.split() for line in result.stdout.splitlines())]
    assert rows and all(row['pid'] in pids for row in rows)
    return rows


def measure(label, groups):
    samples = []
    for _ in range(5):
        rows = [rss(group) for group in groups]
        totals = [sum(x['rssKiB'] for x in group) for group in rows]
        probe.emit('rss-sample', case=label, groups=rows, totalsKiB=totals)
        samples.append(totals)
        time.sleep(.5)
    medians = [statistics.median(x[i] for x in samples) for i in range(len(groups))]
    total = statistics.median(sum(x) for x in samples)
    probe.emit('rss-summary', case=label, medianGroupsKiB=medians, medianTotalKiB=total,
               perConversationMiB=round(total/len(groups)/1024, 3))
    return total/len(groups)/1024


def completed(home, thread):
    for path in (home/'sessions').rglob('*.jsonl'):
        records = []
        for line in path.read_text().splitlines():
            try:
                records.append(json.loads(line))
            except ValueError:
                continue
        if not any(x.get('type') == 'session_meta' and x.get('payload', {}).get('id') == thread for x in records):
            continue
        return sum(x.get('type') == 'event_msg' and x.get('payload', {}).get('type') == 'task_complete' for x in records)
    return 0


def profile(root, name, port):
    base = root/name
    home, ch = base/'h', base/'c'
    home.mkdir(parents=True, mode=0o700)
    ch.mkdir(mode=0o700)
    projects = [base/'a', base/'b']
    for project in projects:
        project.mkdir(mode=0o700)
        subprocess.run(['git', 'init', '-q', str(project)], check=True)
    env = {k: v for k, v in os.environ.items() if k in ('PATH', 'LANG', 'SHELL', 'SDKROOT')}
    env.update(HOME=str(home), CODEX_HOME=str(ch), TMPDIR=str(root), TERM='xterm-256color')
    config = '''model = "gpt-6.1-sol"
model_provider = "probe"
model_reasoning_effort = "low"
approval_policy = "never"
sandbox_mode = "danger-full-access"
[model_providers.probe]
name = "Local synthetic provider"
base_url = "http://127.0.0.1:%d/v1"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = false
[mcp_servers.private-probe]
command = %s
args = [%s,"mcp",%s]
''' % (port, json.dumps(sys.executable), json.dumps(str(Path(probe.__file__).resolve())), json.dumps(str(root)))
    for project in projects:
        config += '\n[projects.%s]\ntrust_level = "trusted"\n' % json.dumps(str(project))
    (ch/'config.toml').write_text(config)
    return base, ch, env, projects


def thread_ids(home):
    ids = set()
    for path in (home/'sessions').rglob('*.jsonl'):
        for line in path.read_text().splitlines():
            try:
                value = json.loads(line)
            except ValueError:
                continue
            if value.get('type') == 'session_meta' and value.get('payload', {}).get('id'):
                ids.add(value['payload']['id'])
                break
    return ids


def first_turn(ui, ch):
    probe.wait('embedded-ready', lambda: 'gpt-6.1-sol' in ui.text().lower())
    prior = {thread for thread in thread_ids(ch) if completed(ch, thread)}
    count = len(probe.calls)
    ui.type('Complete one synthetic memory fixture turn.')
    # This provider can omit tools from a main request. The actual owned
    # rollout task_complete, rather than tools presence, proves the turn ended.
    probe.wait('synthetic-provider-request', lambda: len(probe.calls) > count)
    thread = probe.wait('embedded-completed-rollout', lambda: next((thread for thread in thread_ids(ch)-prior if completed(ch, thread) >= 1), None))
    time.sleep(1)
    probe.emit('embedded-first-turn', completed=True, processAlive=ui.p.poll() is None)
    return thread


def main():
    root = Path(tempfile.mkdtemp(prefix='pa-mem-', dir='/private/tmp'))
    probe.tuis, probe.servers, probe.calls, probe.modes = [], [], [], {}
    model = ThreadingHTTPServer(('127.0.0.1', 0), probe.Model)
    threading.Thread(target=model.serve_forever, daemon=True).start()
    success = False
    try:
        embedded_base, embedded_home, embedded_env, embedded_projects = profile(root, 'e', model.server_port)
        private_base, private_home, private_env, private_projects = profile(root, 'p', model.server_port)
        version = subprocess.run(['codex', '--version'], env=embedded_env, cwd=root, capture_output=True, text=True, check=True).stdout.strip()
        probe.emit('version', version=version)
        embedded = []
        for index in range(2):
            ui = MemoryTUI(embedded_env, embedded_projects[index], None)
            first_turn(ui, embedded_home)
            embedded.append(ui)
            value = measure('embedded-'+str(index+1), [[u.p.pid] for u in embedded])
            if index == 0:
                embedded_one = value
            else:
                embedded_two = value
        # Remove embedded hosts before measuring private hosts to avoid background
        # snapshot/tool activity leaking into the second group of readings.
        for ui in embedded:
            ui.close()
        private, servers = [], []
        for index in range(2):
            endpoint = private_base/(str(index)+'.sock')
            p, _ = probe.start_server(private_env, private_projects[index], endpoint)
            assert p.poll() is None
            servers.append(p)
            measure('private-server-idle-'+str(index+1), [[p.pid]])
            ui = MemoryTUI(private_env, private_projects[index], endpoint)
            time.sleep(2)
            assert ui.p.poll() is None, 'remote-tui-exited'
            probe.emit('remote-attached', index=index, modelVisible='gpt-6.1-sol' in ui.text().lower(),
                       trustVisible='trust' in ui.text().lower(), processAlive=True)
            private.append(ui)
            value = measure('private-attached-'+str(index+1), [[s.p.pid, server.pid] for s, server in zip(private, servers)])
            if index == 0:
                private_one = value
            else:
                private_two = value
        rows = []
        for count, e, p in ((1, embedded_one, private_one), (2, embedded_two, private_two)):
            delta = p-e
            percent = delta/e*100
            rows.append({'conversations': count, 'embeddedMiB': round(e, 3), 'privateMiB': round(p, 3),
                         'deltaMiB': round(delta, 3), 'deltaPercent': round(percent, 2), 'fail': delta > 20 or percent > 10})
        probe.emit('M10-verdict', cases=rows, verdict='FAIL' if any(r['fail'] for r in rows) else 'PASS',
                   privateStage='attached-before-first-turn', pluginsIncluded=False)
        success = True
    except BaseException as exc:
        probe.emit('memory-failure', kind=type(exc).__name__, stage=str(exc) if isinstance(exc, (RuntimeError, AssertionError)) else 'fixture',
                   tui=[{'alive': t.p.poll() is None, 'modelVisible': 'gpt-6.1-sol' in t.text().lower(), 'trustVisible': 'trust' in t.text().lower(), 'screen': t.text()[-3500:].replace(str(root), '<OWNED>')} for t in probe.tuis])
    finally:
        for tui in probe.tuis:
            try:
                tui.close()
            except (OSError, subprocess.TimeoutExpired):
                pass
        for server in probe.servers:
            probe.stop_server(server)
        model.shutdown()
        model.server_close()
        def owned():
            result = subprocess.run(['lsof', '-t', '+D', str(root)], capture_output=True, text=True)
            return {int(x) for x in result.stdout.split()}-{os.getpid()}
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
        probe.emit('memory-cleanup', remaining=len(remaining))
        if not remaining:
            shutil.rmtree(root)
    return 0 if success and not remaining else 1


if __name__ == '__main__':
    sys.exit(main())
