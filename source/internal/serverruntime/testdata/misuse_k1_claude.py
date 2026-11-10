"""Real isolated Haiku PTY; stdin control, safe event labels only."""
import json,os,re,signal,socket,subprocess,sys,threading,time,uuid
from pathlib import Path
import pexpect
SELF=Path(__file__).resolve()
TOOLS={'attach_session','auth_status','send_message','get_session','retire_session','list_sessions'}
def event(root,label,**fields):
    with (root/'safe-events.jsonl').open('a') as f:f.write(json.dumps(dict(label=label,**fields))+'\n')
if len(sys.argv)>1 and sys.argv[1]=='mcp':
    root=Path(sys.argv[2]);bridge=socket.socket(socket.AF_UNIX);bridge.connect(str(root/'b.sock'));cfg=json.loads(bridge.makefile().read());bridge.close()
    env={k:v for k,v in os.environ.items() if k!='ANTHROPIC_AUTH_TOKEN' and k!='CLAUDE_PROJECT_DIR' and (not k.startswith('CLAUDE_CODE_') or k in ('CLAUDE_CODE_SESSION_ID','CLAUDE_CODE_MESSAGING_SOCKET','CLAUDE_CODE_MESSAGING_TOKEN'))}
    child=subprocess.Popen([cfg['binary'],'--broker-url',cfg['endpoint'],'--token',cfg['token'],'--auth-dir',str(root/'state'),'--nickname',cfg['name']],env=env,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
    event(root,'client',pid=child.pid,ppid=os.getpid());pending={}
    def pump():
        try:
            for raw in sys.stdin.buffer:
                q=json.loads(raw)
                if q.get('method')=='tools/call':
                    name=q.get('params',{}).get('name');name=name if name in TOOLS else 'other';pending[str(q.get('id'))]=name;event(root,'model-tool',tool=name)
                child.stdin.write(raw);child.stdin.flush()
        except (OSError,BrokenPipeError):pass
        finally:
            try:child.stdin.close()
            except OSError:pass
    threading.Thread(target=pump,daemon=True).start()
    try:
        for raw in child.stdout:
            q=json.loads(raw);result=q.get('result',{});name=pending.pop(str(q.get('id')),None)
            if 'tools' in result:event(root,'catalog',count=len(result['tools']))
            if name:
                data=result.get('structuredContent',{})
                if not data and result.get('content'):
                    try:data=json.loads(result['content'][0].get('text',''))
                    except ValueError:data={}
                safe={k:data[k] for k in ('registered','name','registrationId','code','accepted') if k in data} if isinstance(data,dict) else {}
                error_text=json.dumps(result.get('content',[])).lower()
                flags=[x for x in ('not found','not-found','unavailable','ready','permission','invalid','validation','registered','schema','unknown','identity','channel','ingress','bound','recipient','sender') if x in error_text] if result.get('isError') or 'error' in q else []
                event(root,'reply',tool=name,ok=not result.get('isError') and 'error' not in q,errorFlags=flags,**safe)
            sys.stdout.buffer.write(raw);sys.stdout.buffer.flush()
    except (BrokenPipeError,OSError):pass
    finally:
        try:child.wait(timeout=5)
        except subprocess.TimeoutExpired:child.terminate();child.wait(timeout=5)
    sys.exit()

cfg=json.loads(sys.stdin.readline());root=Path(cfg['root']);home=root/'home';cc=home/'.claude';project=root/'project'
for path in (home,cc,project,root/'state'):path.mkdir(parents=True,exist_ok=True)
subprocess.run(['git','init','-q',str(project)],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
env={k:v for k,v in os.environ.items() if k in ('PATH','LANG','SDKROOT','SHELL','ANTHROPIC_AUTH_TOKEN')};env.update(HOME=str(home),CLAUDE_CONFIG_DIR=str(cc),TMPDIR='/private/tmp',TERM='xterm-256color')
assert 'CLAUDE_PROJECT_DIR' not in env and not any(k.startswith('CLAUDE_CODE_') for k in env)
bridge=socket.socket(socket.AF_UNIX);bridge.bind(str(root/'b.sock'));os.chmod(root/'b.sock',0o600);bridge.listen()
def bridge_loop():
    while True:
        try:
            c,_=bridge.accept()
            with c:c.sendall(json.dumps(cfg).encode())
        except OSError:return
threading.Thread(target=bridge_loop,daemon=True).start()
mcp=project/'mcp.json';mcp.write_text(json.dumps({'mcpServers':{'x-broker':{'command':sys.executable,'args':[str(SELF),'mcp',str(root)],'env':{'HOME':str(home),'CLAUDE_CONFIG_DIR':str(cc)}}}}))
(cc/'.claude.json').write_text(json.dumps({'hasCompletedOnboarding':True,'theme':'dark','projects':{str(project):{'hasTrustDialogAccepted':True,'allowedTools':[]}}}))
args=['--model','haiku','--effort','low','--strict-mcp-config','--mcp-config',str(mcp),'--tools','','--allowedTools','mcp__x-broker__*','--permission-mode','default','--settings','{"disableAllHooks":true}','--session-id',str(uuid.uuid4())]
p=pexpect.spawn('claude',args,cwd=str(project),env=env,encoding=None,dimensions=(48,160),timeout=1);buffer=bytearray();stopped=False

def drain():
    while not stopped:
        try:
            data=p.read_nonblocking(65536,timeout=.2);buffer.extend(data)
            if len(buffer)>120000:del buffer[:-120000]
            if b'\x1b[6n' in data:p.send(b'\x1b[1;1R')
        except pexpect.TIMEOUT:pass
        except pexpect.EOF:return
threading.Thread(target=drain,daemon=True).start()
def text():return re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]','',buffer.decode(errors='ignore'))
def events():
    path=root/'safe-events.jsonl'
    return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []
def wait(label,condition,seconds):
    until=time.monotonic()+seconds
    while time.monotonic()<until:
        if not p.isalive():raise RuntimeError('host-exited-'+label)
        if condition():return
        time.sleep(.2)
    raise RuntimeError(label)
try:
    def startup():
        t=text()
        if 'Yes, I trust' in t or 'trust this folder' in t.lower():p.send(b'\r');time.sleep(1)
        return any(e['label']=='catalog' for e in events())
    wait('catalog-timeout',startup,60);time.sleep(1);print('READY',flush=True)
    for line in sys.stdin:
        prompt,marker=line.rstrip('\n').rsplit('\t',1)
        buffer.clear();p.send(b'\x1b[200~'+prompt.encode()+b'\x1b[201~');time.sleep(.7);p.send(b'\r')
        wait('model-step-timeout',lambda:marker in text(),180)
        print('DONE',flush=True)
except BaseException as exc:
    flags=[name for name,yes in (('api-error','API Error' in text()),('login',any(x in text().lower() for x in ('log in','sign in'))),('catalog',any(e['label']=='catalog' for e in events()))) if yes]
    label=str(exc) if isinstance(exc,RuntimeError) else type(exc).__name__
    replies=[{k:e[k] for k in ('tool','ok','registered','code','accepted','errorFlags') if k in e} for e in events() if e.get('label')=='reply']
    print('FAIL:'+label+':'+','.join(flags)+':'+json.dumps(replies),flush=True)
finally:
    stopped=True
    if p.isalive():
        try:os.killpg(p.pid,signal.SIGTERM)
        except ProcessLookupError:pass
    p.close(force=True);bridge.close()
    for e in events():
        if e['label']=='client':
            try:os.kill(e['pid'],signal.SIGTERM)
            except ProcessLookupError:pass
